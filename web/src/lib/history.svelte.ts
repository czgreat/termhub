// Conversation history of the CLIs (docs/M10 第 3.2 节): the node reads the
// CLI's own files read-only; the Hub adds which ones this user hid and which
// are open in a session. Lists are cached per profile and refreshed on demand.
import { get, put, del, post } from './api'
import { app, openSession, refreshLists, toast, onUserChange } from './state.svelte'

export type Conv = { id: string; cwd: string; title?: string; first?: string; last?: string; created: number; updated: number; size: number
  /** folderLast(): the newest conversation is hidden from the list */
  hidden?: boolean }
export type Running = { sid: string; cwd: string; conv_id?: string }
export type HiddenFolder = { folder: string; key: string; name?: string; count: number }
export type HistoryData = {
  items: Conv[]; scanned: number; failed: number; incomplete: boolean; fallback: boolean; reason?: string
  /** names the folder the node read (a digest): equal on one node, the same files */
  source?: string
  hidden: number; running: Running[]
  hidden_folders?: HiddenFolder[]; folder_names?: Record<string, string>
  /** newest conversation time per folder in range, hidden ones included */
  latest?: Record<string, { id: string; updated: number }>
}
export type Message = { role: 'user' | 'assistant'; text: string; time?: number; tools?: string[] }
export type Transcript = { id: string; cwd: string; title?: string; messages: Message[]; cursor: number; more: boolean }

type Entry = { data: HistoryData | null; hiddenList: Conv[] | null; loading: boolean; error: string; at: number; failedAt?: number }

export const hist = $state({
  byProfile: {} as Record<number, Entry>,
  /** the conversation shown in the main area (null: the terminals) */
  view: null as null | { profileId: number; conv: Conv },
  showHidden: false,
  query: '',
  /** bumps once a minute so relative times move on */
  tick: 0,
})

setInterval(() => hist.tick++, 60_000)

// Another user logged in (问题单 1 第 1 条): nothing of the one before stays,
// and answers still on their way to the old user's requests are dropped.
let gen = 0
onUserChange(() => {
  gen++
  loading.clear()
  hist.byProfile = {}
  hist.view = null
  hist.showHidden = false
  hist.query = ''
})

/** Profiles whose CLI keeps history the node can read. */
export const historyProfiles = () => app.profiles.filter(p => p.kind === 'claude' || p.kind === 'codex')


function entry(id: number): Entry {
  // read back through the state: what `??=` returns is the plain object, not
  // the reactive proxy, and changes to it would never reach the page
  if (!hist.byProfile[id]) hist.byProfile[id] = { data: null, hiddenList: null, loading: false, error: '', at: 0 }
  return hist.byProfile[id]
}

const loading = new Map<number, Promise<void>>()

/** Reads a profile's list; while one read is under way, another call waits for it. */
export function load(profileId: number, opts: { quiet?: boolean } = {}): Promise<void> {
  if (!profileId) return Promise.resolve()
  const busy = loading.get(profileId)
  if (busy && hist.byProfile[profileId]?.loading) return busy
  const p = read(profileId, opts).finally(() => { if (loading.get(profileId) === p) loading.delete(profileId) })
  loading.set(profileId, p)
  return p
}

async function read(profileId: number, opts: { quiet?: boolean }) {
  const e = entry(profileId), g = gen
  e.loading = true
  if (!opts.quiet) e.error = ''
  try {
    const data = await get<HistoryData>(`/api/profiles/${profileId}/history`)
    if (g !== gen) return
    e.data = data
    e.error = ''
    e.at = Date.now()
    if (hist.showHidden) {
      const hidden = (await get(`/api/profiles/${profileId}/history?hidden=1`)).items ?? []
      if (g === gen) e.hiddenList = hidden
    }
  } catch (err: any) {
    if (g === gen) { e.failedAt = Date.now(); if (!opts.quiet || !e.data) e.error = err.message }
  } finally { e.loading = false }
}

export async function loadHidden(profileId: number) {
  const e = entry(profileId), g = gen
  try {
    const hidden = (await get(`/api/profiles/${profileId}/history?hidden=1`)).items ?? []
    if (g === gen) e.hiddenList = hidden
  } catch (err: any) { toast(err.message) }
}

/**
 * Hides or shows a conversation for this user. Two profiles of one machine can
 * read the same files; the list shows such a conversation once, so it is
 * hidden (or shown) under each of them, or it would come back under the other
 * (复核).
 */
export async function setHidden(profileId: number, conv: Conv, hidden: boolean): Promise<boolean> {
  const pids = await sharingNow(profileId)
  try {
    for (const id of pids) {
      if (hidden) await put(`/api/profiles/${id}/history/${encodeURIComponent(conv.id)}/hidden`)
      else await del(`/api/profiles/${id}/history/${encodeURIComponent(conv.id)}/hidden`)
      const e = entry(id)
      if (e.data) {
        if (hidden) { e.data.items = e.data.items.filter(c => c.id !== conv.id); e.data.hidden++ }
        else if (!e.data.items.some(c => c.id === conv.id)) { e.data.items = [...e.data.items, conv].sort((a, b) => b.updated - a.updated); e.data.hidden = Math.max(0, e.data.hidden - 1) }
      }
      if (e.hiddenList) e.hiddenList = hidden ? [conv, ...e.hiddenList.filter(c => c.id !== conv.id)] : e.hiddenList.filter(c => c.id !== conv.id)
    }
    toast(hidden ? '已隐藏（只是不在列表里显示，文件没有删除）' : '已恢复到列表')
    return true
  } catch (err: any) { toast(err.message); return false }
}

/**
 * The profiles that read the same files as this one (itself included): same
 * machine, same kind of CLI, and the same folder as the node reports it (an
 * older node, which does not: a conversation in common). One not loaded, or
 * whose list failed, is not taken for sharing: a hidden folder must never
 * spread to another account's own files (问题单 1 第 7 条). Claude and Codex
 * never share files, nor do two accounts with their own config folders.
 */
export function sharing(profileId: number): number[] {
  const me = app.profiles.find(p => p.id === profileId)
  const mineData = hist.byProfile[profileId]?.data
  if (!me || !mineData) return [profileId]
  const ids = (id: number) => {
    const e = hist.byProfile[id]
    return new Set([...(e?.data?.items ?? []), ...(e?.hiddenList ?? [])].map(c => c.id).concat(Object.values(e?.data?.latest ?? {}).map(l => l.id)))
  }
  const mine = ids(profileId)
  return historyProfiles().filter(p => {
    if (p.id === profileId) return true
    const d = hist.byProfile[p.id]?.data
    if (p.node_id !== me.node_id || p.kind !== me.kind || !d) return false
    if (d.source && mineData.source) return d.source === mineData.source
    return [...ids(p.id)].some(id => mine.has(id))
  }).map(p => p.id)
}

/**
 * Loads the machine's other profiles of this kind not loaded yet: sharing()
 * needs them to tell. One that failed within the last minute is not asked
 * again (a slow node would hold every action up to its timeout).
 */
async function loadSiblings(profileId: number) {
  const me = app.profiles.find(p => p.id === profileId)
  if (!me) return
  await Promise.all(historyProfiles().filter(p => {
    const e = hist.byProfile[p.id]
    return p.id !== profileId && p.node_id === me.node_id && p.kind === me.kind && !e?.data && !(e?.failedAt && Date.now() - e.failedAt < 60_000)
  }).map(p => load(p.id, { quiet: true })))
}

/** sharing(), once the machine's other profiles of this kind are loaded. */
async function sharingNow(profileId: number): Promise<number[]> {
  await loadSiblings(profileId)
  return sharing(profileId)
}

/** Profiles grouped by the files they read (see sharing), for counting hidden things once. */
export function shareGroups(): number[][] {
  const seen = new Set<number>(), out: number[][] = []
  for (const p of historyProfiles()) {
    if (seen.has(p.id)) continue
    const g = sharing(p.id).filter(id => !seen.has(id))
    g.forEach(id => seen.add(id))
    out.push(g)
  }
  return out
}

export async function readTranscript(profileId: number, id: string, before?: number): Promise<Transcript> {
  return get(`/api/profiles/${profileId}/history/${encodeURIComponent(id)}${before ? '?before=' + before : ''}`)
}

/** Folder identity on Windows, as the Hub normalises it: case, either separator, doubled or trailing ones do not matter. */
export const folderKey = (cwd: string) => cwd.toLowerCase().replace(/\//g, '\\').replace(/^\\\\\?\\/, '').replace(/\\{2,}/g, '\\').replace(/\\+$/, '')
const base = (cwd: string) => cwd.replace(/[\\/]+$/, '').split(/[\\/]/).pop() || cwd


/** A long paste is kept by the CLI inside a wrapper tag: show what was pasted, not the tag. */
export const clean = (s = '') => s.replace(/<\/?pasted_content[^>]*>/g, '').trim()
export const convTitle = (c: Conv) => clean(c.title) || clean(c.first) || clean(c.last) || '（没有文字内容的对话）'

/** "刚刚 / 5 分钟 / 3 小时 / 2 天 / 3 周 / 2 个月 / 1 年", for a list. */
export function ago(unix: number, _tick = hist.tick): string {
  if (!unix) return ''
  const s = Math.max(0, Date.now() / 1000 - unix)
  if (s < 60) return '刚刚'
  if (s < 3600) return Math.floor(s / 60) + ' 分钟'
  if (s < 86400) return Math.floor(s / 3600) + ' 小时'
  if (s < 7 * 86400) return Math.floor(s / 86400) + ' 天'
  if (s < 30 * 86400) return Math.floor(s / (7 * 86400)) + ' 周'
  if (s < 365 * 86400) return Math.floor(s / (30 * 86400)) + ' 个月'
  return Math.floor(s / (365 * 86400)) + ' 年'
}

export const fullTime = (unix: number) => unix ? new Date(unix * 1000).toLocaleString('zh-CN', { hour12: false }) : ''

/**
 * The live session already showing this conversation, if any: under this
 * profile or another reading the same files (问题单 1 第 6 条: the list shows
 * the conversation under one of them only).
 */
export function openSessionFor(profileId: number, convId: string): string {
  if (!convId) return ''
  // the list's running entries are a snapshot up to a minute old: only a
  // session still live (app.sessions, kept current by events) counts (第五轮复核 3)
  const live = new Set(app.sessions.map(s => s.sid))
  for (const id of sharing(profileId)) {
    const sid = hist.byProfile[id]?.data?.running.find(r => r.conv_id === convId && live.has(r.sid))?.sid
    if (sid) return sid
  }
  return ''
}

/** Starts a session in a folder: continue its last conversation, a new one, or one given conversation. */
/** The newest conversation of a folder, as far as the list knows. */
export function lastIn(profileId: number, cwd: string): Conv | undefined {
  const k = folderKey(cwd)
  return hist.byProfile[profileId]?.data?.items.filter(c => folderKey(c.cwd) === k).sort((a, b) => b.updated - a.updated)[0]
}

/**
 * The newest conversation of a folder as "continue" would resume it: the
 * list's newest, or, when a newer one is hidden (alone or with its folder),
 * a stand-in carrying just its time (复核：隐藏后不再提醒).
 */
export function folderLast(profileId: number, cwd: string): Conv | undefined {
  const last = lastIn(profileId, cwd)
  const t = hist.byProfile[profileId]?.data?.latest?.[folderKey(cwd)]
  return t && t.updated > (last?.updated ?? 0) ? { id: t.id, cwd, updated: t.updated, created: t.updated, size: 0, hidden: true } : last
}

/** Written to within this many seconds and not open here: probably still going on elsewhere. */
export const busyWindow = 5 * 60
export function busyElsewhere(profileId: number, c: Conv, _tick = hist.tick): boolean {
  if (Date.now() / 1000 - c.updated >= busyWindow || openSessionFor(profileId, c.id)) return false
  // a termhub session of this profile runs in that folder (started new or
  // "continue", so its conversation id is not known): most likely it is this
  // one, carried on here, not somewhere else
  const k = folderKey(c.cwd)
  const live = new Set(app.sessions.map(s => s.sid))
  return !sharing(profileId).some(id => hist.byProfile[id]?.data?.running.some(r => !r.conv_id && live.has(r.sid) && folderKey(r.cwd) === k))
}

/** Before deciding, a list older than a minute is read again (with those sharing its files): the warning is about the last minutes. */
export async function fresh(profileId: number) {
  const old = (id: number) => { const e = hist.byProfile[id]; return !e?.at || Date.now() - e.at > 60_000 }
  // this one and the machine's others not loaded yet first: whether they
  // share its files is known only then (第二轮复核 4)
  await Promise.all([old(profileId) ? load(profileId, { quiet: true }) : undefined, loadSiblings(profileId)])
  await Promise.all(sharing(profileId).filter(old).map(id => load(id, { quiet: true })))
}

/**
 * The same conversation carried on in two places at once (say the node's own
 * terminal and here) breaks both: two CLIs append to one file. termhub cannot
 * see a CLI started outside it, so a conversation written to a few minutes ago
 * asks first.
 */
function confirmBusy(profileId: number, c: Conv): boolean {
  if (!busyElsewhere(profileId, c)) return true
  return confirm(`这段对话 ${ago(c.updated)}${ago(c.updated) === '刚刚' ? '' : '前'}还有新内容，可能正在别处进行（比如那台电脑自己的终端）。\n\n` +
    '同一段对话同时在两处继续，两边都会出问题。请先在那边退出，再在这里继续。\n\n仍要在这里继续吗？')
}

/**
 * This user's live session of the same kind of AI CLI on the same machine in
 * a folder, under whichever profile: only one may run there, the Hub refuses
 * a second (同一文件夹只开一个; Claude Code and Codex apart). From
 * app.sessions, which events keep current. '' when none, and for a plain shell.
 */
export function openInFolder(profileId: number, cwd: string): string {
  const p = app.profiles.find(p => p.id === profileId)
  if (!p || (p.kind !== 'claude' && p.kind !== 'codex')) return ''
  const k = folderKey(cwd), me = app.me?.user.id
  return app.sessions.find(s => s.owner_id === me && s.node_id === p.node_id && folderKey(s.cwd) === k
    && s.kind === p.kind)?.sid ?? ''
}
export const folderTaken = '这个文件夹里已经有一个运行中的会话（同一个文件夹同时只能开一个），已切换过去'

export async function start(profileId: number, cwd: string, mode: 'new' | 'continue' | 'resume', conv?: string) {
  if (mode !== 'new') await fresh(profileId)
  // already open here: go there, never a second CLI on the same file, nor
  // a second one in the folder
  const same = mode === 'resume' && conv ? openSessionFor(profileId, conv) : ''
  const open = same || openInFolder(profileId, cwd)
  if (open) { if (!same) toast(folderTaken); openSession(open); hist.view = null; app.drawer = false; return }
  const target = mode === 'resume' ? hist.byProfile[profileId]?.data?.items.find(c => c.id === conv)
    : mode === 'continue' ? folderLast(profileId, cwd) : undefined
  if (target && !confirmBusy(profileId, target)) return
  try {
    const r = await post('/api/sessions', { profile_id: profileId, cwd, cols: 120, rows: 32, mode, resume_session: conv ?? '' })
    await refreshLists()
    openSession(r.session.sid)
    hist.view = null
    app.drawer = false
    load(profileId, { quiet: true })
  } catch (e: any) { toast(e.message) }
}

// ---- by machine (docs/M10 第 3.5 节) ----
// The sidebar lists machines, most recently used first; each shows its two
// newest conversations until opened, then its folders and their conversations.

export type ConvRef = Conv & { profileId: number }
export type Folder = { key: string; profileId: number; cwd: string; name: string; items: ConvRef[]; total: number; updated: number; running: boolean }
export type Machine = {
  nodeId: number; name: string; online: boolean; updated: number
  profiles: { id: number; name: string; kind: string; my_folders?: string[] }[]
  recent: ConvRef[]; folders: Folder[]; loading: boolean; errors: string[]; notes: string[]; count: number
}

/** Loads every history profile not loaded yet (or all, forced). */
export function loadAll(opts: { force?: boolean; quiet?: boolean } = {}) {
  for (const p of historyProfiles()) {
    const e = hist.byProfile[p.id]
    if (opts.force || (!e?.at && !e?.error)) load(p.id, { quiet: opts.quiet })
  }
}

export function machines(query: string): Machine[] {
  const q = query.trim().toLowerCase()
  const out = new Map<number, Machine>()
  // Two profiles of one machine may read the same files (no config dir of
  // their own): each conversation is listed once, under the profile that saw
  // it newest (复核：同一段对话显示两次).
  const owner = new Map<string, { pid: number; updated: number }>()
  for (const p of historyProfiles()) {
    for (const c of hist.byProfile[p.id]?.data?.items ?? []) {
      const k = p.node_id + ':' + c.id, o = owner.get(k)
      if (!o || c.updated > o.updated) owner.set(k, { pid: p.id, updated: c.updated })
    }
  }
  for (const p of historyProfiles()) {
    const n = app.nodes.find(n => n.id === p.node_id)
    let m = out.get(p.node_id)
    if (!m) out.set(p.node_id, m = { nodeId: p.node_id, name: n?.name ?? `节点 ${p.node_id}`, online: !!n?.online, updated: 0,
      profiles: [], recent: [], folders: [], loading: false, errors: [], notes: [], count: 0 })
    m.profiles.push({ id: p.id, name: p.name, kind: p.kind, my_folders: p.my_folders })
    const e = hist.byProfile[p.id]
    if (e?.loading) m.loading = true
    if (e?.error) m.errors.push(`${p.name}：${e.error}`)
    const d = e?.data
    if (!d) continue
    if (d.fallback || (d.reason && !d.items.length)) m.notes.push(`${p.name}：${d.reason}`)
    const running = new Set(d.running.map(r => folderKey(r.cwd)))
    const byDir = new Map<string, Folder>()
    for (const c of d.items) {
      if (owner.get(p.node_id + ':' + c.id)?.pid !== p.id) continue
      const dk = folderKey(c.cwd || '?')
      const total = byDir.get(dk)
      if (total) total.total++ // counted before the search filter: "hide this folder" names all of it
      else if (q) byDir.set(dk, { key: `${p.id}|${dk}`, profileId: p.id, cwd: c.cwd, name: '', items: [], total: 1, updated: 0, running: false })
      if (q && ![c.title, c.first, c.last, c.cwd, d.folder_names?.[folderKey(c.cwd)]].some(s => s?.toLowerCase().includes(q))) continue
      const k = folderKey(c.cwd || '?')
      let f = byDir.get(k)
      if (!f) byDir.set(k, f = { key: `${p.id}|${k}`, profileId: p.id, cwd: c.cwd, name: '', items: [], total: 1, updated: 0, running: false })
      if (!f.name) {
        f.name = d.folder_names?.[k] || (c.cwd ? base(c.cwd) : '（目录未知）')
        f.running = running.has(k)
      }
      const ref = { ...c, profileId: p.id }
      f.items.push(ref)
      f.updated = Math.max(f.updated, c.updated)
      m.count++
    }
    m.folders.push(...[...byDir.values()].filter(f => f.items.length))
  }
  const list = [...out.values()]
  for (const m of list) {
    for (const f of m.folders) f.items.sort((a, b) => b.updated - a.updated)
    // two folders of the same name on one machine: add their parent folder
    const seen = new Map<string, number>()
    for (const f of m.folders) seen.set(f.name, (seen.get(f.name) ?? 0) + 1)
    // (the same path under two profiles is told apart by the profile's label, not here)
    const paths = new Map<string, Set<string>>()
    for (const f of m.folders) paths.set(f.name, (paths.get(f.name) ?? new Set()).add(folderKey(f.cwd)))
    for (const f of m.folders) if ((seen.get(f.name) ?? 0) > 1 && (paths.get(f.name)?.size ?? 0) > 1 && f.cwd && !hist.byProfile[f.profileId]?.data?.folder_names?.[folderKey(f.cwd)]) {
      f.name = f.cwd.replace(/[\\/]+$/, '').split(/[\\/]/).slice(-2).join('\\')
    }
    m.folders.sort((a, b) => b.updated - a.updated)
    m.updated = m.folders[0]?.updated ?? 0
    m.recent = m.folders.flatMap(f => f.items).sort((a, b) => b.updated - a.updated).slice(0, 2)
  }
  // a machine this user never had a conversation on is left out, once known
  return list.filter(m => m.count > 0 || m.loading || m.errors.length || (!q && m.notes.length) || m.profiles.some(p => !hist.byProfile[p.id]?.at))
    .sort((a, b) => b.updated - a.updated || a.name.localeCompare(b.name))
}

/** Hide or rename a folder in this user's list; the folder itself is never touched. */
/** Hide or rename a folder in this user's list, under every profile of the machine that lists it; the folder itself is never touched. */
export async function setFolder(profileId: number, cwd: string, change: { hidden?: boolean; name?: string }): Promise<boolean> {
  const pids = await sharingNow(profileId)
  try {
    for (const id of pids) await put(`/api/profiles/${id}/history-folder`, { folder: cwd, ...change })
    if (change.hidden !== undefined) toast(change.hidden ? '文件夹已隐藏（文件都还在，可在“已隐藏”里恢复）' : '文件夹已恢复到列表')
    return true
  } catch (e: any) { toast(e.message); return false }
  finally { await Promise.all(pids.map(id => load(id, { quiet: true }))) } // also after a failure half way: show what the Hub has
}
