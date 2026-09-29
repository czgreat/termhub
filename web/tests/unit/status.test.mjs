// Unit tests of the session status dots (主人 2026-09-25: 多开时看不出哪个做完了)
// with the page's own state module and a fake Hub. No browser, no node, no CLI.
import { test } from 'node:test'
import assert from 'node:assert/strict'

const store = new Map()
globalThis.localStorage = { getItem: k => store.get(k) ?? null, setItem: (k, v) => store.set(k, String(v)), removeItem: k => store.delete(k) }
globalThis.window = globalThis
let focused = true
globalThis.document = { visibilityState: 'visible', addEventListener() {}, hasFocus: () => focused }
globalThis.setInterval = () => 0

let sessions = []
let late = null // when set, /api/sessions answers with it once the test says so
const routes = {
  '/api/me': () => ({ user: { id: 1, username: 'u1', role: 'user' }, csrf: 'c', pending: [] }),
  '/api/nodes': () => ({ nodes: [] }), '/api/profiles': () => ({ profiles: [] }),
  '/api/sessions': () => late ? late : ({ sessions }),
}
globalThis.fetch = async path => {
  const h = routes[path]
  const body = h ? await h() : { code: 'not_found' }
  return { ok: !!h, status: h ? 200 : 404, statusText: '', text: async () => JSON.stringify(body) }
}

const { app, loadMe, refreshLists, openSession, closeTab, statusOf, plainTitle, busyTitle } = await import('../../src/lib/state.svelte.ts')
const row = (sid, title) => ({ sid, title, profile_id: 1, owner_id: 1, cwd: 'C:\\p', node_id: 1 })
async function titles(t) { sessions = Object.entries(t).map(([sid, title]) => row(sid, title)); await refreshLists() }

test('Claude Code title glyphs read as working or waiting', () => {
  assert.equal(busyTitle('◐ 修复登录'), true)
  assert.equal(busyTitle('⠋ Working'), true)
  assert.equal(busyTitle('✳ 修复登录'), false)
  assert.equal(busyTitle('pwsh'), false)
  assert.equal(plainTitle('◑ 修复登录'), '修复登录')
  assert.equal(plainTitle('✳ 修复登录'), '修复登录')
  assert.equal(plainTitle('C:\\work'), 'C:\\work')
})

test('a session that finishes out of view is done until looked at', async () => {
  await loadMe()
  await titles({ a: '◐ a', b: '◐ b' })
  openSession('a') // a is in view, b is not
  assert.equal(statusOf('a'), 'busy')
  assert.equal(statusOf('b'), 'busy')

  await titles({ a: '✳ a', b: '✳ b' })
  assert.equal(statusOf('a'), '', 'finished while watched: nothing to point out')
  assert.equal(statusOf('b'), 'done')

  openSession('b')
  assert.equal(statusOf('b'), '', 'looking at it clears the dot')
})

test('finishing while the window is in the background counts as unseen', async () => {
  await titles({ a: '◐ a', b: '✳ b' })
  focused = false
  await titles({ a: '✳ a', b: '✳ b' })
  focused = true
  assert.equal(statusOf('a'), 'done')
  assert.equal(statusOf('b'), '', 'idle from the start: never marked')
})

test('an option menu on screen is ask, and working again clears done', async () => {
  await titles({ a: '◐ a', b: '◐ b' })
  openSession('a')
  await titles({ a: '✳ a', b: '✳ b' })
  assert.equal(statusOf('b'), 'done')
  app.menus.b = { kind: 'single', options: [] }
  assert.equal(statusOf('b'), 'ask')
  app.menus.b = null
  await titles({ a: '✳ a', b: '◓ b' })
  assert.equal(statusOf('b'), 'busy')
  await titles({ a: '✳ a' })
  assert.equal(statusOf('b'), '', 'a session that is gone has no dot')
  assert.equal(app.done.b, undefined)
})

test('working seen only in stale answers still ends as done (状态点复核 3)', async () => {
  await titles({ a: '✳ a', b: '✳ b' })
  openSession('a')
  let answer
  late = new Promise(r => { answer = r })
  const stale = refreshLists() // asked while b works; the answer comes back after a newer one
  await new Promise(r => setTimeout(r, 0))
  late = null
  await titles({ a: '✳ a', b: '✳ b' }) // the newer answer: b idle, never seen working
  answer({ sessions: [row('a', '✳ a'), row('b', '◐ b')] })
  await stale
  await titles({ a: '✳ a', b: '✳ b' })
  assert.equal(statusOf('b'), 'done')
})

test("the tab that takes a closed tab's place counts as seen (状态点复核 4)", async () => {
  await titles({ a: '◐ a', b: '◐ b' })
  openSession('b'); openSession('a')
  await titles({ a: '◐ a', b: '✳ b' })
  assert.equal(statusOf('b'), 'done')
  closeTab('a')
  assert.equal(app.panes[0].active, 'b')
  assert.equal(statusOf('b'), '')
})
