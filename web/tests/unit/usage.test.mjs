// Unit tests of the cost and context figures (主人 2026-09-26: 按官方计费折算美元、上下文占用).
import { test } from 'node:test'
import assert from 'node:assert/strict'

globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} }
globalThis.window = globalThis
globalThis.document = { visibilityState: 'visible', addEventListener() {}, hasFocus: () => true }
globalThis.setInterval = () => 0

let sessions = [], asked = 0
const routes = {
  '/api/me': () => ({ user: { id: 1, username: 'u1', role: 'user' }, csrf: 'c', pending: [] }),
  '/api/nodes': () => ({ nodes: [] }), '/api/profiles': () => ({ profiles: [] }),
  '/api/sessions': () => ({ sessions }),
  '/api/sessions/usage': () => { asked++; return { sessions: { a: { cost: 117.47, context_pct: 37.2, model: 'claude-opus-5-5', unpriced: ['deepseek-v4-flash'] } } } },
}
globalThis.fetch = async path => {
  const h = routes[path]
  const body = h ? h() : { code: 'not_found' }
  return { ok: !!h, status: h ? 200 : 404, statusText: '', text: async () => JSON.stringify(body) }
}

const { fmtCost, usageText, usageTip } = await import('../../src/lib/usage.ts')
const { app, loadMe, refreshLists, refreshUsage } = await import('../../src/lib/state.svelte.ts')

test('dollars and context share, no token counts', () => {
  assert.equal(fmtCost(0.004), '<$0.01')
  assert.equal(fmtCost(3.4199), '$3.42')
  assert.equal(fmtCost(1234.5), '$1,235')
  assert.equal(fmtCost(null), '')
  assert.equal(usageText({ cost: 3.42, context_pct: 37.6 }), '$3.42 · 38%')
  assert.equal(usageText({ cost: null, context_pct: 12 }), '12%', 'no price: only the context')
  assert.equal(usageText(undefined), '')
  const tip = usageTip({ cost: 3.42, context_pct: 37.6, model: 'gpt-6-sol', unpriced: ['x'] })
  assert.match(tip, /官方 API 价格折算：\$3\.42/)
  assert.match(tip, /没有价格、未计入：x/)
  assert.doesNotMatch(tip, /token/i)
})

test('figures are asked for only when an AI session runs', async () => {
  await loadMe()
  sessions = [{ sid: 'sh', kind: 'shell', title: '' }]
  await refreshLists()
  await refreshUsage()
  assert.equal(asked, 0, 'a plain shell has no figures to ask for')
  sessions = [{ sid: 'a', kind: 'claude', title: '' }]
  await refreshLists() // a new AI session: asked at once, not at the next round
  await new Promise(r => setTimeout(r, 0))
  assert.equal(asked, 1)
  assert.equal(app.usage.a.cost, 117.47)
})
