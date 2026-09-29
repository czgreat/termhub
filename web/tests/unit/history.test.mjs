// Unit tests of the history state and the input box (问题单 1 第 1、6、7 条, 第二轮复核 1、4、7, 第三轮复核 1) with the page's own
// modules and a fake Hub (fetch). No browser, no node, no CLI.
// Run: node --import ./tests/unit/register.mjs --test "tests/unit/*.test.mjs"
import { test } from 'node:test'
import assert from 'node:assert/strict'

const store = new Map()
globalThis.localStorage = { getItem: k => store.get(k) ?? null, setItem: (k, v) => store.set(k, String(v)), removeItem: k => store.delete(k) }
globalThis.window = globalThis
globalThis.document = { visibilityState: 'visible', addEventListener() {} }
globalThis.confirm = () => true
globalThis.setInterval = () => 0 // the minute tick would keep the test process alive

// The fake Hub: path -> handler; a handler may return a promise to answer late.
let routes = {}
const calls = []
globalThis.fetch = async (path, init) => {
  calls.push(`${init?.method ?? 'GET'} ${path}`)
  const h = routes[`${init?.method ?? 'GET'} ${path}`] ?? routes[path]
  const body = h ? await h(init) : { code: 'not_found', msg: 'no route' }
  const status = h ? 200 : 404
  return { ok: status < 300, status, statusText: '', text: async () => JSON.stringify(body) }
}

const { app, loadMe, refreshLists, restoreLayout } = await import('../../src/lib/state.svelte.ts')
const { terms } = await import('../../src/lib/terms.ts')
const { sendDraft, resolveRecovery } = await import('../../src/lib/composer.ts')
// Legacy fixtures model definite admission/rejection; real delayed replies are tested below.
function put(sid, t) {
  terms.set(sid, { ...t,
    pasteBatch: async text => { const drops = t.drops(); t.paste(text); return { status: drops === t.drops() ? 'accepted' : 'rejected', acceptedBytes: 0 } },
    inputBatch: async text => { t.input(text); return { status: 'accepted', acceptedBytes: text.length } },
  })
}

const H = await import('../../src/lib/history.svelte.ts')
const { hist } = H

const list = (items, extra = {}) => ({ items, scanned: items.length, failed: 0, incomplete: false, fallback: false, hidden: 0, running: [], ...extra })
const conv = (id, cwd, updated = 100) => ({ id, cwd, title: id, created: updated, updated, size: 1 })
const profile = (id, node = 1, kind = 'claude') => ({ id, node_id: node, name: 'p' + id, kind })
const me = id => () => ({ user: { id, username: 'u' + id, role: 'user' }, csrf: 'c', pending: [] })
const tick = () => new Promise(r => setTimeout(r, 0))
const live = (...sids) => sids.map(sid => ({ sid, profile_id: 0, owner_id: 0, cwd: '', node_id: 1 })) // app.sessions: still running

test('another user logging in sees nothing of the one before', async () => {
  routes = { '/api/me': me(1), '/api/profiles/1/history': () => list([conv('a-secret', 'C:\\a')]) }
  await loadMe()
  app.profiles = [profile(1)]
  H.loadAll(); await tick(); await tick()
  assert.equal(H.machines('')[0].recent[0].id, 'a-secret')
  app.drafts['s1'] = 'draft of A'

  // A's next request is still on its way when B logs in
  let answerA
  routes['/api/profiles/1/history'] = () => new Promise(r => { answerA = () => r(list([conv('a-late', 'C:\\a')])) })
  const pending = H.load(1, { quiet: true })
  await tick()
  routes = { '/api/me': me(2), '/api/profiles/1/history': () => list([conv('b-own', 'C:\\b')]) }
  await loadMe()
  assert.deepEqual(app.profiles, [])
  assert.deepEqual({ ...app.drafts }, {})
  assert.deepEqual(Object.keys(hist.byProfile), [])
  app.profiles = [profile(1)]
  calls.length = 0
  H.loadAll(); await tick(); await tick()
  assert.ok(calls.includes('GET /api/profiles/1/history'), 'B reads the list again')
  answerA(); await pending; await tick()
  const ids = H.machines('').flatMap(m => m.folders.flatMap(f => f.items.map(c => c.id)))
  assert.deepEqual(ids, ['b-own'])
})

test('an unloaded or separate profile is not taken for sharing; the same source is', async () => {
  app.profiles = [profile(10), profile(11), profile(12)]
  hist.byProfile = {}
  const puts = []
  routes = {
    '/api/profiles/10/history': () => list([conv('x1', 'C:\\work')], { source: 'aaa' }),
    '/api/profiles/11/history': () => list([conv('y1', 'C:\\work')], { source: 'bbb' }), // its own config folder
    '/api/profiles/12/history': () => list([conv('x1', 'C:\\work')], { source: 'aaa' }),
  }
  for (const id of [10, 11, 12]) routes[`PUT /api/profiles/${id}/history-folder`] = () => { puts.push(id); return { ok: true } }
  await H.load(10)
  assert.deepEqual(H.sharing(10), [10], 'the others are not loaded yet: not sharing')
  assert.equal(await H.setFolder(10, 'C:\\work', { hidden: true }), true)
  assert.deepEqual(puts.sort(), [10, 12], 'hidden where the same files are read, not under the other account')
  // an older node without a source: a conversation in common decides
  hist.byProfile[11].data.source = undefined
  hist.byProfile[10].data.source = undefined
  assert.deepEqual(H.sharing(10), [10, 12])
})

test('a conversation open under a profile sharing the files is found', async () => {
  app.profiles = [profile(20), profile(21)]
  hist.byProfile = {}
  routes = {
    '/api/profiles/20/history': () => list([conv('c1', 'C:\\w', Date.now() / 1000 - 3600)], { source: 's' }),
    '/api/profiles/21/history': () => list([conv('c1', 'C:\\w', Date.now() / 1000 - 3600)], { source: 's', running: [{ sid: 'sid-b', cwd: 'C:\\w', conv_id: 'c1' }] }),
  }
  await H.load(20); await H.load(21)
  app.sessions = live('sid-b')
  assert.equal(H.openSessionFor(20, 'c1'), 'sid-b')
  calls.length = 0
  await H.start(20, 'C:\\w', 'resume', 'c1')
  assert.ok(!calls.some(c => c.startsWith('POST /api/sessions')), 'no second CLI on the same conversation')
  assert.equal(app.panes[0].active, 'sid-b')
})

test('a list answer for the user before is not taken after another logged in', async () => {
  routes = { '/api/me': me(5), '/api/nodes': () => ({ nodes: [] }), '/api/sessions': () => ({ sessions: [] }),
    '/api/profiles': () => ({ profiles: [profile(1)] }) }
  await loadMe()
  let answer
  routes['/api/profiles'] = () => new Promise(r => { answer = () => r({ profiles: [{ ...profile(99), env: { SECRET: 'of user 5' } }] }) })
  const late = refreshLists()
  await tick()
  routes = { '/api/me': me(6), '/api/nodes': () => ({ nodes: [] }), '/api/sessions': () => ({ sessions: [] }), '/api/profiles': () => ({ profiles: [] }) }
  await loadMe()
  answer(); await late
  assert.deepEqual(app.profiles, [], 'user 6 holds nothing of user 5')
})

test('continue checks a sharing profile not loaded yet before starting a second CLI', async () => {
  app.profiles = [profile(30), profile(31)]
  hist.byProfile = {}
  const old = Date.now() / 1000 - 3600
  routes = {
    '/api/profiles/30/history': () => list([conv('c9', 'C:\w', old)], { source: 's' }),
    '/api/profiles/31/history': () => list([conv('c9', 'C:\w', old)], { source: 's', running: [{ sid: 'sid-31', cwd: 'C:\w', conv_id: 'c9' }] }),
    'POST /api/sessions': () => ({ session: { sid: 'new' } }),
  }
  await H.load(30) // 31 not loaded
  app.sessions = live('sid-31')
  calls.length = 0
  await H.start(30, 'C:\w', 'resume', 'c9')
  assert.ok(calls.includes('GET /api/profiles/31/history'), 'the other profile is read first')
  assert.ok(!calls.includes('POST /api/sessions'), 'no second CLI')
  assert.equal(app.panes[0].active, 'sid-31')
})

test('what is typed while the input box is sending stays for the next message', async () => {
  const sent = []
  put('t1', { canSend: () => true, refuses: () => false, readOnly: () => false, drops: () => 0, paste: t => sent.push('paste:' + t), input: d => sent.push(d), selection: () => '', files() {}, focus() {}, appCursor: () => false })
  app.drafts['t1'] = 'first message'
  const first = sendDraft('t1')
  assert.equal(await sendDraft('t1'), false, 'a second press during the first does nothing')
  app.drafts['t1'] = 'next message' // typed during the 300 ms pause
  assert.equal(await first, true)
  assert.deepEqual(sent, ['paste:first message', '\r'])
  assert.equal(app.drafts['t1'], 'next message')
  // not connected: nothing sent, the text stays
  put('t2', { ...terms.get('t1'), canSend: () => false })
  app.drafts['t2'] = 'keep me'
  assert.equal(await sendDraft('t2'), false)
  assert.equal(app.drafts['t2'], 'keep me')
})

// The input box with a real SessionLink (第三轮复核 1): input refused after a
// queue was dropped half sent must not take the draft, nor send an Enter that
// would submit the torn command.
class FakeWS {
  static OPEN = 1
  static last = null
  readyState = 0
  sent = []
  constructor() { FakeWS.last = this }
  send(d) { this.sent.push(typeof d === 'string' ? d : new TextDecoder().decode(d)) }
  close() { this.readyState = 3 }
  open() { this.readyState = 1; this.onopen() }
  attached() { this.onmessage({ data: JSON.stringify({ t: 'attached', mode: 'resume', from: 0, cols: 80, rows: 24 }) }) }
  text() { return this.sent.filter(d => !d.startsWith('{')) }
}
const sleep = ms => new Promise(r => setTimeout(r, ms))

test('the input box keeps its text while input is refused, and sends it once taken again', async () => {
  globalThis.WebSocket = FakeWS
  globalThis.location = { protocol: 'https:', host: 'x' }
  const { SessionLink } = await import('../../src/lib/session.ts')
  const l = new SessionLink('t3', { attached() {}, output() {}, control() {}, state() {} })
  put('t3', { canSend: () => l.canSend, refuses: () => l.refuses, readOnly: () => l.readOnly, drops: () => l.drops, paste: t => l.input(t), input: d => l.input(d), selection: () => '', files() {}, focus() {}, appCursor: () => false })
  l.input('echo OLD_PARTIAL'); await sleep(300); l.input('\r')
  FakeWS.last.open(); FakeWS.last.attached() // the text goes, the Enter waits
  l.input(new Uint8Array(1 << 20)) // overflow half sent: input refused
  app.drafts['t3'] = 'echo NEW_DRAFT'
  assert.equal(await sendDraft('t3'), false)
  assert.equal(app.drafts['t3'], 'echo NEW_DRAFT', 'the draft stays')
  await sleep(400)
  assert.deepEqual(FakeWS.last.text(), ['echo OLD_PARTIAL'], 'no Enter after the torn command')
  l.unblock() // the user has looked and said so
  assert.equal(await sendDraft('t3'), true)
  assert.deepEqual(FakeWS.last.text(), ['echo OLD_PARTIAL', 'echo NEW_DRAFT', '\r'])
  l.close()
})

test('a queue dropped while the input box sends: the text goes back and no Enter follows', async () => {
  const sent = []
  let drops = 0
  put('t4', { canSend: () => true, refuses: () => false, readOnly: () => false, drops: () => drops, paste: t => { sent.push('paste:' + t); drops++ }, input: d => sent.push(d), selection: () => '', files() {}, focus() {}, appCursor: () => false })
  app.drafts['t4'] = 'lost on the way'
  assert.equal(await sendDraft('t4'), false)
  assert.deepEqual(sent, ['paste:lost on the way'])
  assert.equal(app.drafts['t4'], 'lost on the way')
})

// 第四轮复核 1: the input box online, the page frozen for two minutes during
// its own pause before the Enter (not the link's queue): no Enter after it.
test('a page frozen during the input box pause does not submit the text late', async () => {
  const { SessionLink } = await import('../../src/lib/session.ts')
  const l = new SessionLink('t5', { attached() {}, output() {}, control() {}, state() {} })
  l.connect(); FakeWS.last.open(); FakeWS.last.attached()
  const now = Date.now
  let skew = 0
  Date.now = () => now() + skew
  try {
    put('t5', { canSend: () => l.canSend, refuses: () => l.refuses, readOnly: () => l.readOnly, drops: () => l.drops, paste: t => { l.input(t); skew = 120_000 }, input: d => l.input(d), selection: () => '', files() {}, focus() {}, appCursor: () => false })
    app.drafts['t5'] = 'echo NEW_DRAFT'
    assert.equal(await sendDraft('t5'), false)
    assert.deepEqual(FakeWS.last.text(), ['echo NEW_DRAFT'], 'the text is on the command line, no Enter')
    assert.equal(app.drafts['t5'], '', 'not put back: it is in the terminal already')
    // frozen after the Esc of a long press: nothing else goes, the text stays in the box
    skew = 0
    delete app.recovery['t5'] // user inspected the terminal before discarding the copy
    put('t5', { ...terms.get('t5'), input: d => { l.input(d); if (d === '\x1b') skew = 120_000 } })
    app.drafts['t5'] = 'echo AFTER_ESC'
    assert.equal(await sendDraft('t5', true), false)
    assert.deepEqual(FakeWS.last.text(), ['echo NEW_DRAFT', '\x1b'])
    assert.equal(app.drafts['t5'], 'echo AFTER_ESC')
  } finally { Date.now = now; l.close() }
})

// 第四轮复核 2 and one AI CLI of a kind per folder: B (sharing A's files)
// already runs in the folder; starting from A goes there, no second CLI.
test('starting where a session of the same kind runs goes to it', async () => {
  app.profiles = [profile(40), profile(41)]
  app.panes = [{ tabs: [], active: '' }]
  hist.byProfile = {}
  const recent = Date.now() / 1000 - 30
  routes = {
    '/api/profiles/40/history': () => list([conv('c7', 'C:\\w', recent)], { source: 's' }),
    '/api/profiles/41/history': () => list([conv('c7', 'C:\\w', recent)], { source: 's', running: [{ sid: 'sid-41', cwd: 'C:\\w', conv_id: 'c7' }] }),
    'POST /api/sessions': () => ({ session: { sid: 'duplicate' } }),
  }
  // the live sessions (events keep app.sessions current, not the list's snapshot)
  const me = app.me?.user.id
  const sess = (sid, profile_id, cwd, conv_id = '', owner_id = me) => ({ sid, profile_id, owner_id, cwd, conv_id, node_id: 1, kind: app.profiles.find(p => p.id === profile_id)?.kind ?? 'claude' })
  app.sessions = [sess('sid-41', 41, 'C:\\w', 'c7')]
  let asked = 0
  globalThis.confirm = () => { asked++; return true }
  try {
    await H.fresh(40)
    assert.equal(H.openInFolder(40, 'C:\\w'), 'sid-41')
    calls.length = 0
    await H.start(40, 'C:\\w', 'continue')
    assert.ok(!calls.includes('POST /api/sessions'), 'no second CLI')
    assert.equal(app.panes[0].active, 'sid-41')
    assert.equal(asked, 0)
    // one of this kind in the folder, whatever it runs: the only one there
    // (同一文件夹只开一个), for a new conversation or another one resumed too
    app.sessions = [sess('sid-41b', 41, 'c:\\W\\')]
    assert.equal(H.openInFolder(40, 'C:\\w'), 'sid-41b')
    app.sessions = [sess('old', 42, 'C:\\w', 'c-older')]
    app.profiles = [profile(40), profile(41), profile(42)]
    assert.equal(H.openInFolder(40, 'C:\\w'), 'old', 'any profile of the kind on the machine')
    calls.length = 0
    await H.start(40, 'C:\\w', 'new')
    assert.ok(!calls.includes('POST /api/sessions'))
    assert.equal(app.panes[0].active, 'old')
    // not taken for it: another folder, Codex beside Claude Code, another
    // machine, a plain shell, another user's session, one just ended
    assert.equal(H.openInFolder(40, 'C:\\other'), '')
    app.profiles = [profile(40), profile(41), profile(43, 1, 'codex'), profile(44, 2), profile(45, 1, 'custom')]
    app.sessions = [sess('codex', 43, 'C:\\w'), { ...sess('elsewhere', 44, 'C:\\w'), node_id: 2 }, sess('theirs', 41, 'C:\\w', '', me + 1)]
    assert.equal(H.openInFolder(40, 'C:\\w'), '')
    app.sessions = [sess('shell', 45, 'C:\\w')]
    assert.equal(H.openInFolder(45, 'C:\\w'), '', 'a plain shell is not limited')
    app.sessions = []
    assert.equal(H.openInFolder(40, 'C:\\w'), '')
  } finally { globalThis.confirm = () => true; app.sessions = [] }
})

// One person at the keyboard: while someone else has it, the input box keeps
// its text; taken over halfway through a send, no Enter follows.
test('the input box keeps its text while the session is read-only', async () => {
  const { SessionLink } = await import('../../src/lib/session.ts')
  const events = []
  const l = new SessionLink('t6', { attached() {}, output() {}, control: m => events.push(m.t), state() {} })
  l.connect(); FakeWS.last.open(); FakeWS.last.attached()
  const driver = on => FakeWS.last.onmessage({ data: JSON.stringify({ t: 'driver', on, by: on ? '' : '管理员 root', cols: 80, rows: 24 }) })
  put('t6', { canSend: () => l.canSend, refuses: () => l.refuses, readOnly: () => l.readOnly, drops: () => l.drops, paste: t => { l.input(t); driver(false) }, input: d => l.input(d), selection: () => '', files() {}, focus() {}, appCursor: () => false })
  try {
    driver(false)
    assert.equal(l.canSend, false)
    app.drafts['t6'] = 'echo WAIT'
    assert.equal(await sendDraft('t6'), false)
    assert.equal(app.drafts['t6'], 'echo WAIT')
    l.input('typed')
    assert.deepEqual(FakeWS.last.text(), [])
    assert.ok(events.includes('input_readonly'))
    driver(true) // given back; then taken over during the pause before the Enter
    assert.equal(await sendDraft('t6'), false)
    assert.deepEqual(FakeWS.last.text(), ['echo WAIT'], 'no Enter once someone else has the keyboard')
    assert.equal(app.drafts['t6'], '', 'on the command line already: not back in the box (第五轮复核 1)')
    delete app.recovery['t6'] // user inspected the terminal before discarding the copy
    driver(true) // taken back: sending the box again would not repeat the text
    put('t6', { ...terms.get('t6'), paste: t => l.input(t) })
    app.drafts['t6'] = 'next'
    assert.equal(await sendDraft('t6'), true)
    assert.deepEqual(FakeWS.last.text(), ['echo WAIT', 'next', '\r'])
  } finally { l.close() }
})

// 第五轮复核 3: the list's running entry is a snapshot; the session ended
// since, resuming reopens the conversation instead of going to a dead tab.
test('an ended session named by a fresh list is not switched to', async () => {
  app.profiles = [profile(50)]
  app.panes = [{ tabs: [], active: '' }]
  hist.byProfile = {}
  const old = Date.now() / 1000 - 3600
  routes = {
    '/api/profiles/50/history': () => list([conv('c5', 'C:\\w', old)], { running: [{ sid: 'ended-session', cwd: 'C:\\w', conv_id: 'c5' }] }),
    'POST /api/sessions': () => ({ session: { sid: 'reopened' } }),
    '/api/sessions': () => ({ sessions: [] }), '/api/nodes': () => ({ nodes: [] }), '/api/profiles': () => ({ profiles: [profile(50)] }),
  }
  await H.load(50)
  app.sessions = [] // it ended: events took it out
  calls.length = 0
  await H.start(50, 'C:\\w', 'resume', 'c5')
  assert.ok(calls.includes('POST /api/sessions'), 'the conversation is reopened')
  assert.equal(app.panes[0].active, 'reopened')
})

// Opus 复核: the paste left, but the Hub refused it (taken over on the way):
// the text is not on the command line, so it goes back into the box.
test('a paste refused by the Hub goes back into the box', async () => {
  const { SessionLink } = await import('../../src/lib/session.ts')
  const l = new SessionLink('t7', { attached() {}, output() {}, control() {}, state() {} })
  l.connect(); FakeWS.last.open(); FakeWS.last.attached()
  const hub = m => FakeWS.last.onmessage({ data: JSON.stringify(m) })
  put('t7', { canSend: () => l.canSend, refuses: () => l.refuses, readOnly: () => l.readOnly, drops: () => l.drops,
    paste: t => { l.input(t); hub({ t: 'driver', on: false, by: '管理员 root' }); hub({ t: 'err', code: 'read_only' }) },
    input: d => l.input(d), selection: () => '', files() {}, focus() {}, appCursor: () => false })
  try {
    app.drafts['t7'] = 'echo REFUSED'
    assert.equal(await sendDraft('t7'), false)
    assert.equal(app.drafts['t7'], 'echo REFUSED')
    assert.deepEqual(FakeWS.last.text(), ['echo REFUSED'], 'sent, but refused by the Hub; no Enter')
  } finally { l.close() }
})


test('F08 session creation kind survives profile change or deletion', async () => {
 routes={'/api/me':me(89)};await loadMe()
 app.profiles=[profile(81),profile(82,1,'codex')]
 app.sessions=[{sid:'snapshot',profile_id:82,node_id:1,kind:'claude',cwd:'C:\\w',owner_id:app.me.user.id}]
 assert.equal(H.openInFolder(81,'C:\\w'),'snapshot')
 app.profiles=[profile(81)]
 assert.equal(H.openInFolder(81,'C:\\w'),'snapshot')
})

test('F12 stale refresh cannot mark a recovered live session ended', async () => {
 let answerOld
 routes={'/api/me':me(90),'/api/nodes':()=>({nodes:[]}),'/api/profiles':()=>({profiles:[]}),'/api/sessions':()=>new Promise(r=>{answerOld=r})}
 await loadMe()
 app.sessions=live('restored'); app.panes=[{tabs:['restored'],active:'restored'}]
 const old=refreshLists();await tick()
 routes['/api/sessions']=()=>({sessions:live('restored')})
 await refreshLists()
 answerOld({sessions:[]});await old
 assert.equal(app.sessions[0]?.sid,'restored');assert.equal(app.ended.restored,undefined)
 app.ended.restored={sid:'restored'}
 await refreshLists();assert.equal(app.ended.restored,undefined)
})

test('F09 delayed rejection restores only that batch; unknown results keep a separate copy', async () => {
 const {SessionLink}=await import('../../src/lib/session.ts')
 const l=new SessionLink('late',{attached(){},output(){},control(){},state(){}})
 l.connect();const ws=FakeWS.last;ws.open();ws.onmessage({data:JSON.stringify({t:'attached',mode:'resume',from:0,input_ack:true})})
 terms.set('late',{canSend:()=>l.canSend,refuses:()=>l.refuses,readOnly:()=>l.readOnly,drops:()=>l.drops,pasteBatch:t=>l.inputBatch(t),inputBatch:t=>l.inputBatch(t)})
 const batch=()=>ws.sent.filter(d=>d.startsWith('{')).map(d=>JSON.parse(d)).filter(m=>m.t==='input_batch').at(-1)
 app.drafts.late='first'
 const sent=sendDraft('late');await sleep(350)
 assert.equal(resolveRecovery('late',true),false);assert.equal(resolveRecovery('late',false),false)
 assert.equal(app.recovery.late,'first')
 app.drafts.late='next'
 ws.onmessage({data:JSON.stringify({t:'input_result',re:batch().id,code:'read_only'})})
 assert.equal(await sent,false);assert.equal(app.drafts.late,'first\nnext');assert.equal(app.recovery.late,undefined)
 app.drafts.late='uncertain'
 const lost=sendDraft('late');l.detach();assert.equal(await lost,false)
 assert.equal(app.drafts.late,'');assert.equal(app.recovery.late,'uncertain')
 assert.equal(await sendDraft('late'),false,'uncertain input is not resent')
 l.close()
})


test('F09 legacy-compatible delayed read-only refusal preserves the draft', async () => {
 let readonly=false, drops=0, resolve
 const rejected=new Promise(r=>{resolve=r})
 const paste=()=>{readonly=true;setTimeout(()=>{drops++;resolve({status:'rejected',acceptedBytes:0})},350)}
 const t={canSend:()=>true,refuses:()=>false,readOnly:()=>readonly,drops:()=>drops,paste,input(){throw new Error('must not submit')},pasteBatch:()=>{paste();return rejected},inputBatch(){throw new Error('must not submit')}}
 terms.set('compat-delay',t);app.drafts['compat-delay']='must survive'
 const result=sendDraft('compat-delay');await sleep(400);await result
 assert.equal(app.drafts['compat-delay'],'must survive')
})


test('F12 startup waits for the applied snapshot before restoring saved tabs', async () => {
 const answers=[]
 routes={'/api/me':me(110),'/api/nodes':()=>({nodes:[]}),'/api/profiles':()=>({profiles:[]}),'/api/sessions':()=>new Promise(r=>answers.push(r))}
 await loadMe()
 store.set('termhub.layout.110',JSON.stringify({panes:[{tabs:['one'],active:'one'},{tabs:['two'],active:'two'}],split:'row',focus:1}))
 const initial=refreshLists(), event=refreshLists();await tick()
 answers[0]({sessions:live('one','two')});await initial
 assert.equal(app.listsUser,0,'superseded request must not trigger layout restoration')
 answers[1]({sessions:live('one','two')});await event
 assert.equal(app.listsUser,110);restoreLayout()
 assert.deepEqual(app.panes.map(p=>[...p.tabs]),[['one'],['two']])
})


test('G4 ended and closed sessions keep reachable recovery and restored drafts', async () => {
  const { render } = await import('svelte/server')
  const { default: Sidebar } = await import('../../src/components/Sidebar.svelte')
  const { closeTab } = await import('../../src/lib/state.svelte.ts')
  globalThis.__TH_VERSION__ = 'test'
  routes = { '/api/me': me(114) }; await loadMe()
  app.sessions = []; app.panes = [{ tabs: ['gone'], active: 'gone' }]
  app.recovery = { gone: 'G4 preserved copy' }; app.drafts = { gone: 'my own draft' }
  closeTab('gone')
  const html = () => render(Sidebar, { props: { onadmin() {} } }).body
  assert.match(html(), /G4 preserved copy/)
  assert.match(html(), /放回草稿/)
  assert.match(html(), /清除副本/)
  assert.equal(resolveRecovery('gone', true), true)
  assert.equal(app.drafts.gone, 'G4 preserved copy\nmy own draft')
  assert.match(html(), /G4 preserved copy/)
  assert.match(html(), /保存的草稿/)
  assert.equal(JSON.parse(store.get('termhub.drafts.114')).gone, app.drafts.gone)
  app.recovery.gone = 'discard this only'
  assert.equal(resolveRecovery('gone', false), true)
  assert.equal(app.drafts.gone, 'G4 preserved copy\nmy own draft')
  assert.equal(JSON.parse(store.get('termhub.recovery.114')).gone, undefined)
})
