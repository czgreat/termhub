// Acknowledged admission to the Hub queue; never infer success from a timer.
import { app, saveDrafts, saveRecovery, toast } from './state.svelte'
import { terms } from './terms'
import type { InputResult } from './session'
const sending = new Set<string>()
export async function sendDraft(sid: string, interrupt = false): Promise<boolean> {
  const t = terms.get(sid)
  if (!t || sending.has(sid)) return false
  if (app.recovery[sid]) { toast('上次发送结果待确认，请先检查终端和保留副本'); return false }
  if (t.readOnly() || t.refuses() || !t.canSend()) { toast('终端暂时不能接收输入，内容留在输入框里'); return false }
  const user = app.me?.user.id
  const text = app.drafts[sid] ?? ''
  app.sending[sid] = true
  app.drafts[sid] = ''
  if (text) app.recovery[sid] = text
  saveDrafts(true); saveRecovery()
  sending.add(sid)
  const began = Date.now(), drops = t.drops()
  const current = () => app.me?.user.id === user
  const restore = () => {
    if (!current()) return
    const next = app.drafts[sid] ?? ''
    app.drafts[sid] = text && next ? text + '\n' + next : text || next
    delete app.recovery[sid]; saveDrafts(true); saveRecovery()
  }
  const uncertain = () => { if (current()) toast('发送未完成或结果不明，已保留副本；请先检查终端，避免重复发送') }
  const late = () => Date.now() - began > 5000
  const ready = () => current() && !late() && t.canSend() && !t.readOnly() && !t.refuses() && t.drops() === drops
  const pause = (ms: number) => new Promise(r => setTimeout(r, ms))
  let pasted = false
  try {
    if (interrupt) {
      const result = await t.inputBatch('\x1b')
      if (result.status !== 'accepted') { restore(); return false }
      await pause(400)
    }
    if (!ready()) { restore(); return false }
    if (text) {
      const result: InputResult = await t.pasteBatch(text)
      if (result.status !== 'accepted') {
        if (result.status === 'rejected' && result.acceptedBytes === 0) restore()
        else uncertain()
        return false
      }
      pasted = true
      await pause(300)
    }
    if (!ready()) { if (!pasted) restore(); else uncertain(); return false }
    const result = await t.inputBatch('\r')
    if (result.status !== 'accepted') { uncertain(); return false }
    if (current()) { delete app.recovery[sid]; saveRecovery() }
    return true
  } catch {
    uncertain(); return false
  } finally { sending.delete(sid); if (current()) delete app.sending[sid] }
}

// A pending send still owns its recovery copy; the UI cannot restore it twice.
export function resolveRecovery(sid: string, restore: boolean): boolean {
  if (app.sending[sid] || !app.recovery[sid]) return false
  if (restore) app.drafts[sid] = app.recovery[sid] + (app.drafts[sid] ? '\n' + app.drafts[sid] : '')
  delete app.recovery[sid]; saveDrafts(true); saveRecovery()
  return true
}
