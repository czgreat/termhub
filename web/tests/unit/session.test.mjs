// Unit tests of the input queue of SessionLink (问题单 1 第 3、4 条, 第二轮
// 复核 2、3、6, 第三轮复核 1、2): a fake WebSocket and a fake clock, no browser, no node, no CLI.
// Run: npm run test:unit
import { test, mock } from 'node:test'
import assert from 'node:assert/strict'

class FakeWS {
  static OPEN = 1
  static last = null
  static made = 0
  readyState = 0
  sent = [] // [time, text]
  constructor() { FakeWS.last = this; FakeWS.made++ }
  send(d) { this.sent.push([Date.now(), typeof d === 'string' ? d : new TextDecoder().decode(d)]) }
  close() { this.readyState = 3 }
  open() { this.readyState = 1; this.onopen() }
  msg(m) { this.onmessage({ data: JSON.stringify(m) }) }
  attached() { this.msg({ t: 'attached', mode: 'resume', from: 0, cols: 80, rows: 24 }) }
  input() { return this.sent.filter(([, d]) => !d.startsWith('{')).map(([t, d]) => [t, d]) }
  text() { return this.input().map(([, d]) => d) }
}
globalThis.WebSocket = FakeWS
globalThis.window = globalThis
globalThis.location = { protocol: 'https:', host: 'x' }

const { SessionLink } = await import('../../src/lib/session.ts')

test('G3 not_running ends input without a partial-input confirmation', () => {
  const { l, events } = link()
  l.connect(); const ws = FakeWS.last; ws.open(); ws.attached()
  ws.msg({ t: 'err', code: 'not_running' })
  l.input('must not send\r')
  assert.equal(l.refuses, false)
  assert.equal(l.canSend, false)
  assert.deepEqual(events, [])
  assert.deepEqual(ws.text(), [])
  l.close()
})

test('G3 repeated stalled errors do not announce another blocked transition', () => {
  const { l, events } = link()
  l.connect(); const ws = FakeWS.last; ws.open(); ws.attached()
  ws.msg({ t: 'err', code: 'input_stalled' })
  ws.msg({ t: 'err', code: 'input_stalled' })
  assert.deepEqual(events, ['input_dropped:partial'])
  assert.equal(l.canSend, false)
  l.unblock(); ws.msg({ t: 'err', code: 'input_stalled' })
  assert.deepEqual(events, ['input_dropped:partial', 'input_dropped:partial'])
  l.close()
})

function link() {
  const events = []
  const l = new SessionLink('s', { attached() {}, output() {}, control: m => { if (m.t.startsWith('input_')) events.push(m.t + (m.partial ? ':partial' : '')) }, state() {} })
  return { l, events }
}
const steps = (ms) => { for (let i = 0; i < ms / 50; i++) mock.timers.tick(50) } // the clock moves with the timers
function clock(fn) {
  return () => {
    mock.timers.enable({ apis: ['setTimeout', 'Date'], now: 0 })
    try { fn() } finally { mock.timers.reset() }
  }
}

test('a command whose start is too old is not sent in part', clock(() => {
  const { l, events } = link()
  mock.timers.tick(1000); l.input('echo ')
  mock.timers.tick(31000); l.input('hello\r')
  mock.timers.tick(30000)
  FakeWS.last.open(); FakeWS.last.attached()
  assert.deepEqual(FakeWS.last.text(), [])
  assert.deepEqual(events, ['input_dropped'])
  l.input('ls\r') // what is typed afterwards goes out as usual
  assert.deepEqual(FakeWS.last.text(), ['ls\r'])
}))

test('an overflow before anything went drops everything and refuses input until attached', clock(() => {
  const { l, events } = link()
  l.input('rm -rf ./build')
  l.input(new Uint8Array(1 << 20))
  l.input('\r')
  FakeWS.last.open(); FakeWS.last.attached()
  assert.deepEqual(FakeWS.last.text(), [])
  assert.deepEqual(events, ['input_dropped'])
  l.input('ok\r') // attached again: taken
  assert.deepEqual(FakeWS.last.text(), ['ok\r'])
}))

test('an overflow while detached reconnects at once', clock(() => {
  const { l } = link()
  l.connect(); FakeWS.last.open(); FakeWS.last.attached()
  l.detach()
  const before = FakeWS.made
  l.input(new Uint8Array((1 << 20) + 1))
  assert.equal(FakeWS.made, before + 1)
}))

test('an overflow while the queue is half sent blocks input until the user says so', clock(() => {
  const { l, events } = link()
  l.input('echo prefix'); mock.timers.tick(300); l.input('\r')
  FakeWS.last.open(); FakeWS.last.attached() // 'echo prefix' goes, the Enter waits 300 ms
  l.input(new Uint8Array(1 << 20)) // a huge paste during the pause
  l.input('\r')
  steps(1000)
  assert.deepEqual(FakeWS.last.text(), ['echo prefix'], 'the torn command is never submitted')
  assert.deepEqual(events, ['input_dropped:partial', 'input_blocked'])
  l.unblock()
  l.input('\x15') // the user clears the line (Ctrl+U) and goes on
  assert.deepEqual(FakeWS.last.text(), ['echo prefix', '\x15'])
}))

test('a half-sent queue older than a minute is not finished, nor is new input behind it', clock(() => {
  const { l, events } = link()
  l.input('x'); mock.timers.tick(300); l.input('\r')
  const first = FakeWS.last
  first.open(); first.attached() // 'x' goes out, the Enter waits 300 ms
  l.detach()
  l.input('entirely new command\r')
  mock.timers.tick(120_000)
  l.poke()
  FakeWS.last.open(); FakeWS.last.attached()
  assert.deepEqual(first.text(), ['x'])
  assert.deepEqual(FakeWS.last.text(), [])
  assert.deepEqual(events, ['input_dropped:partial'])
}))

test('a half-sent queue within the minute is finished on the next attach', clock(() => {
  const { l, events } = link()
  l.input('x'); mock.timers.tick(300); l.input('\r')
  const first = FakeWS.last
  first.open(); first.attached()
  l.detach()
  mock.timers.tick(5000)
  l.poke()
  FakeWS.last.open(); FakeWS.last.attached()
  steps(1000)
  assert.deepEqual(FakeWS.last.text(), ['\r'])
  assert.deepEqual(events, [])
}))

test('the pauses before Enter and after Esc are kept when sending late', clock(() => {
  const { l, events } = link()
  l.input('\x1b'); mock.timers.tick(400)
  l.input('\x1b[200~text\x1b[201~'); mock.timers.tick(300)
  l.input('\r')
  l.input('a'); l.input('b') // typed keys: no pause needed
  mock.timers.tick(5000)
  FakeWS.last.open(); FakeWS.last.attached()
  const t0 = Date.now()
  steps(1000)
  const sent = FakeWS.last.input().map(([t, d]) => [t - t0, d])
  assert.deepEqual(sent, [[0, '\x1b'], [400, '\x1b[200~text\x1b[201~'], [700, '\r'], [700, 'a'], [700, 'b']])
  assert.deepEqual(events, [])
}))

test('attached again during a pause keeps the pause', clock(() => {
  const { l } = link()
  l.input('text'); mock.timers.tick(300); l.input('\r')
  FakeWS.last.open(); FakeWS.last.attached()
  const t0 = Date.now()
  mock.timers.tick(100)
  FakeWS.last.attached() // a gap or the node back: attached once more
  steps(1000)
  assert.deepEqual(FakeWS.last.input().map(([t, d]) => [t - t0, d]), [[0, 'text'], [300, '\r']])
}))

test('input while the queue is going out waits its turn', clock(() => {
  const { l } = link()
  l.input('x'); mock.timers.tick(300); l.input('\r')
  FakeWS.last.open(); FakeWS.last.attached()
  l.input('y') // during the pause before the Enter
  steps(1000)
  assert.deepEqual(FakeWS.last.text(), ['x', '\r', 'y'])
}))

test('the node away with the link up: input waits for it and goes once attached again', clock(() => {
  const { l } = link()
  l.connect(); FakeWS.last.open(); FakeWS.last.attached()
  assert.equal(l.canSend, true)
  FakeWS.last.msg({ t: 'node_offline' })
  assert.equal(l.canSend, false)
  l.input('ls\r')
  assert.deepEqual(FakeWS.last.text(), [])
  mock.timers.tick(10_000)
  FakeWS.last.attached() // node_online, then attach
  assert.deepEqual(FakeWS.last.text(), ['ls\r'])
}))

test('a page frozen during a pause does not send a stale Enter (第三轮复核 2)', clock(() => {
  const { l, events } = link()
  l.input('echo STALE'); mock.timers.tick(300); l.input('\r')
  FakeWS.last.open(); FakeWS.last.attached() // the text goes, the Enter waits 300 ms
  mock.timers.setTime(Date.now() + 120_000) // frozen: the clock moved, no timer ran
  steps(1000)
  assert.deepEqual(FakeWS.last.text(), ['echo STALE'])
  assert.deepEqual(events, ['input_dropped:partial'])
  assert.equal(l.canSend, false, 'input waits for the user')
}))

test('input refused shows in canSend', clock(() => {
  const { l } = link()
  l.connect(); FakeWS.last.open(); FakeWS.last.attached()
  l.detach()
  l.input(new Uint8Array((1 << 20) + 1)) // overflow before anything went: refused until attached
  assert.equal(l.refuses, true)
  FakeWS.last.open(); FakeWS.last.attached()
  assert.equal(l.refuses, false)
  assert.equal(l.canSend, true)
}))

test('taken over by someone else: input refused, what waits dropped, given back: taken again', clock(() => {
  const { l, events } = link()
  l.connect(); FakeWS.last.open(); FakeWS.last.attached()
  l.detach()
  l.input('x\r') // waits for the link, which reconnects at once
  FakeWS.last.open()
  FakeWS.last.msg({ t: 'driver', on: false, by: '管理员 root' })
  FakeWS.last.attached()
  l.input('ls\r')
  steps(1000)
  assert.deepEqual(FakeWS.last.text(), [])
  assert.equal(l.canSend, false)
  assert.deepEqual(events.filter(e => e !== 'input_readonly'), ['input_dropped'])
  assert.ok(events.includes('input_readonly'))
  FakeWS.last.msg({ t: 'driver', on: true, by: '' })
  l.input('ok\r')
  assert.deepEqual(FakeWS.last.text(), ['ok\r'])
}))


test('F05 host input rejection blocks further Enter until inspected', () => {
 const {l,events}=link();l.connect();FakeWS.last.open();FakeWS.last.attached()
 FakeWS.last.msg({t:'err',code:'input_stalled'});l.input('\r')
 assert.equal(l.refuses,true);assert.deepEqual(FakeWS.last.text(),[]);assert.ok(events.includes('input_blocked'));l.close()
})

test('F09 acknowledgement identifies partial batches and ignores another ID', async () => {
 const {l}=link();l.connect();const ws=FakeWS.last;ws.open();ws.msg({t:'attached',mode:'resume',from:0,input_ack:true})
 const result=l.inputBatch('x'.repeat(65537))
 const last=()=>ws.sent.map(([,d])=>{try{return JSON.parse(d)}catch{return {}}}).filter(m=>m.t==='input_batch').at(-1)
 const first=last();ws.msg({t:'input_result',re:first.id+99,code:'read_only'})
 assert.equal(last().id,first.id)
 ws.msg({t:'input_result',re:first.id});await Promise.resolve()
 ws.msg({t:'input_result',re:last().id,code:'read_only'})
 assert.deepEqual(await result,{status:'rejected',acceptedBytes:65536});assert.equal(l.refuses,true);l.close()
})


test('F09 takeover between chunks also blocks a partially admitted batch', async () => {
 const {l,events}=link();l.connect();const ws=FakeWS.last;ws.open();ws.msg({t:'attached',mode:'resume',from:0,input_ack:true})
 const result=l.inputBatch('x'.repeat(65537))
 const first=JSON.parse(ws.sent.at(-1)[1]);ws.msg({t:'input_result',re:first.id});ws.msg({t:'driver',on:false})
 assert.deepEqual(await result,{status:'rejected',acceptedBytes:65536})
 ws.msg({t:'driver',on:true});l.input('\r');assert.equal(l.refuses,true);assert.ok(events.includes('input_blocked'));assert.deepEqual(ws.text(),[]);l.close()
})
