// Sensitive operations need a fresh second factor (docs/M6 第 8 节). Any call
// that fails with reverify_required opens the dialog and is retried once.
import { ApiError, post } from './api'
import { loadMe } from './state.svelte'

export const reverify = $state({ open: false, error: '' })
let waiting: { resolve: (ok: boolean) => void } | null = null

export async function withReverify<T>(fn: () => Promise<T>): Promise<T> {
  try {
    return await fn()
  } catch (e) {
    if (!(e instanceof ApiError) || e.code !== 'reverify_required') throw e
    const ok = await new Promise<boolean>(resolve => { waiting = { resolve }; reverify.open = true; reverify.error = '' })
    if (!ok) throw e
    return await fn()
  }
}

export async function submitReverify(code: string) {
  try {
    await post('/api/auth/reverify', { code })
    await loadMe()
    reverify.open = false
    waiting?.resolve(true)
  } catch (e: any) {
    reverify.error = e?.message ?? String(e)
  }
}

export function cancelReverify() {
  reverify.open = false
  waiting?.resolve(false)
}
