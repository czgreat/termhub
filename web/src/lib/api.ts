// Thin client for the Hub's REST API (docs/M6 第 6 节: every write carries the
// CSRF token; the browser adds Origin and the cookies by itself).

export class ApiError extends Error {
  constructor(public status: number, public code: string, msg: string, public body: any = {}) {
    super(msg)
  }
}

let csrf = ''
export function setCSRF(token: string) { csrf = token }

// Called when any request finds the login gone (expired while the phone was
// in the background, revoked elsewhere): the page shows the login form instead
// of failing quietly (PWA 复核).
let unauthorized: (() => void) | null = null
export function onUnauthorized(fn: () => void) { unauthorized = fn }

export async function api<T = any>(method: string, path: string, body?: unknown, raw?: BodyInit, headers: Record<string, string> = {}): Promise<T> {
  const h: Record<string, string> = { ...headers }
  if (method !== 'GET' && method !== 'HEAD') h['X-TH-CSRF'] = csrf
  let payload: BodyInit | undefined = raw
  if (body !== undefined) {
    h['Content-Type'] = 'application/json'
    payload = JSON.stringify(body)
  }
  const resp = await fetch(path, { method, headers: h, body: payload, credentials: 'same-origin' })
  const text = await resp.text()
  let data: any = {}
  try { data = text ? JSON.parse(text) : {} } catch { data = { raw: text } }
  if (resp.status === 401 && data.code === 'unauthorized') unauthorized?.()
  if (!resp.ok) throw new ApiError(resp.status, data.code ?? 'http_' + resp.status, data.msg ?? resp.statusText, data)
  return data as T
}

export const get = <T = any>(path: string) => api<T>('GET', path)
export const post = <T = any>(path: string, body?: unknown) => api<T>('POST', path, body ?? {})
export const put = <T = any>(path: string, body?: unknown) => api<T>('PUT', path, body ?? {})
export const patch = <T = any>(path: string, body?: unknown) => api<T>('PATCH', path, body ?? {})
export const del = <T = any>(path: string) => api<T>('DELETE', path)

export type User = { id: number; username: string; display_name: string; role: string; status: string; totp_confirmed: boolean; must_change_password: boolean }
export type Me = { user: User; csrf: string; reverified: boolean; pending: string[]; server_time: number }
export type Node = { id: number; name: string; note: string; status: string; online: boolean; fingerprint_mismatch: boolean; agent_ver: string; host_ver: string; pwsh_store: boolean; position: number }
export type Profile = { id: number; node_id: number; name: string; kind: string; mode: string; shell_path: string; shell_args: string[]; command: string; args: string[]; resume_cmd: string; continue_cmd: string; my_folders?: string[]; env?: Record<string, string>; default_cwd: string; idle_timeout: number; quote_style: string; status: string }
export type Session = { sid: string; node_id: number; profile_id: number; profile_name: string; kind: string; owner_id: number; cwd: string; title: string; created_at: number; ended_at: number; end_reason: string; exit_code: number | null; conv_id?: string }
export type Entry = { name: string; dir: boolean; link?: boolean; size: number; modified: number; hidden?: boolean }
export type Listing = { path: string; parent?: string; entries: Entry[]; page: number; pages: number; total: number }
export type Drive = { letter: string; kind: string; label?: string; total?: number; free?: number }

export async function sha256Hex(data: ArrayBuffer): Promise<string> {
  const d = await crypto.subtle.digest('SHA-256', data)
  return Array.from(new Uint8Array(d)).map(b => b.toString(16).padStart(2, '0')).join('')
}

/** Sends a file's bytes in chunks (docs/M4 第 5 节); returns the SHA-256 for finish. */
export async function uploadChunks(file: Blob, begin: { id: string; next: number; chunk: number }, chunkURL: string, onProgress?: (sent: number) => void) {
  const bytes = await file.arrayBuffer()
  let off = begin.next
  while (off < bytes.byteLength) {
    const piece = bytes.slice(off, Math.min(off + begin.chunk, bytes.byteLength))
    try {
      const r = await api<{ next: number }>('PUT', chunkURL + '?offset=' + off, undefined, piece, { 'Content-Type': 'application/octet-stream' })
      off = r.next
    } catch (e) {
      if (e instanceof ApiError && e.code === 'bad_offset') { off = e.body.next; continue } // the Hub says where to resume
      throw e
    }
    onProgress?.(off)
  }
  return sha256Hex(bytes)
}
