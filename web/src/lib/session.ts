// One attached terminal session: the browser side of /ws/session/{sid}
// (docs/M1 4.1 to 4.3, 5.2). It keeps `have`, the end offset it has received,
// so that a reconnect resumes instead of replaying, and re-attaches on a gap.

export type Attached = { mode: 'resume' | 'replay'; from: number; cols: number; rows: number; prelude?: string; alt_screen?: boolean }
export type InputResult = { status: 'accepted' | 'rejected' | 'unknown'; acceptedBytes: number }
export type Ctl = { t: string; [k: string]: any }

export interface SessionEvents {
  attached(a: Attached): void
  output(data: Uint8Array): void
  control(m: Ctl): void
  state(s: 'connecting' | 'open' | 'closed' | 'detached'): void
}

const maxPending = 1 << 20 // bytes waiting for the link
const maxPendingAge = 60_000 // ms: older input is not sent late
const maxPause = 500 // ms: the longest pause kept when sending late

export class SessionLink {
  private inputAck = false
  private batchID = 0
  private batches = new Map<number, (code: string) => void>()
  private ws: WebSocket | null = null
  private have: number | null = null
  private closed = false
  private attempt = 0
  private ready = false // attached on the current connection
  private timer: number | undefined
  // Input typed while the link is down (a phone back from the background, a
  // network change, the node away) waits here and goes out, in order, once
  // the session is attached again, instead of being dropped without a word.
  // Bounded in size and age: nothing typed more than a minute ago is sent,
  // and all or nothing: sending the end of a command whose start was dropped
  // could run something else (问题单 1 第 3 条, 第二轮复核 2、3).
  private pending: { bytes: Uint8Array; at: number }[] = []
  private pendingBytes = 0
  private sentSome = false // part of the queue is out: the rest belongs to it
  private dropping = false // the queue overflowed before anything went: refuse input until attached again
  private blocked = false // the queue was dropped half sent: refuse input until the user says so (unblock)
  private nodeDown = false // the browser's link is up but the node is away: input waits (第二轮复核 6)
  private pumpTimer: number | undefined
  readOnly = false // someone else is at the keyboard (the Hub's driver message): input is not taken
  drops = 0 // queues dropped so far: a sender that sees it change knows its input may be gone
  cols = 80
  rows = 24

  constructor(readonly sid: string, private ev: SessionEvents) {}

  connect() {
    if (this.closed) return
    this.ev.state('connecting')
    const url = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ws/session/' + this.sid
    const ws = new WebSocket(url)
    ws.binaryType = 'arraybuffer'
    this.ws = ws
    ws.onopen = () => {
      this.attempt = 0
      this.ev.state('open')
      this.attach()
    }
    ws.onmessage = e => {
      if (e.data instanceof ArrayBuffer) {
        const v = new DataView(e.data)
        const start = Number(v.getBigUint64(0))
        this.accept(start, new Uint8Array(e.data, 8))
        return
      }
      const m: Ctl = JSON.parse(e.data)
      if (m.t === 'attached') {
        this.inputAck = !!m.input_ack
        const a = m as unknown as Attached
        if (a.mode === 'replay') this.have = a.from
        this.ev.attached(a)
        this.flush()
      } else if (m.t === 'input_result') {
        this.batches.get(m.re)?.(m.code || '')
      } else if (m.t === 'gap') {
        this.attach() // ask again from what we have
      } else if (m.t === 'err' && m.code === 'read_only') {
        this.drops++ // the Hub refused input already on its way (taken over meanwhile): a sender puts its text back
      } else if (m.t === 'exited' || (m.t === 'err' && m.code === 'not_running')) {
        // G3: an ended process cannot accept more input; there is no line to unblock.
        this.pending = []; this.pendingBytes = 0; this.sentSome = false
        this.blocked = false; this.dropping = false
        this.close()
      } else if (m.t === 'err' && ['input_overflow', 'input_stalled'].includes(m.code)) {
        if (!this.blocked) this.drop(true)
      } else if (m.t === 'driver') {
        this.readOnly = !m.on
        if (this.readOnly && this.pending.length) this.drop() // it would be refused on the way
      } else if (m.t === 'node_offline' || (m.t === 'err' && m.code === 'node_offline')) {
        this.nodeOffline() // until the node is back and the session attached again
      }
      this.ev.control(m)
    }
    ws.onclose = () => {
      this.unknownBatches()
      this.ws = null
      this.ready = false
      this.ev.state('closed')
      this.scheduleReconnect()
    }
    ws.onerror = () => ws.close()
  }

  private accept(start: number, data: Uint8Array) {
    const end = start + data.length
    if (this.have === null) return // nothing attached yet
    if (end <= this.have) return // repeat
    if (start > this.have) { this.attach(); return } // hole: re-attach from have
    const fresh = data.subarray(this.have - start)
    this.have = end
    this.ev.output(fresh)
  }

  /** Sends attach with the current size and what we already have. */
  attach() {
    this.send({ t: 'attach', have: this.have, cols: this.cols, rows: this.rows })
  }

  resize(cols: number, rows: number) {
    this.cols = cols
    this.rows = rows
    this.send({ t: 'resize', cols, rows })
  }

  redraw() { this.send({ t: 'redraw' }) }

  input(data: string | Uint8Array) {
    const bytes = typeof data === 'string' ? new TextEncoder().encode(data) : data
    if (this.closed) return
    if (this.readOnly) { this.ev.control({ t: 'input_readonly' }); return }
    if (this.blocked) { this.ev.control({ t: 'input_blocked' }); return }
    if (this.dropping) return
    if (!this.up || this.pending.length) {
      if (this.pendingBytes + bytes.length > maxPending) { this.overflow(); return }
      this.pending.push({ bytes, at: Date.now() })
      this.pendingBytes += bytes.length
      if (!this.ws) this.poke() // detached or waiting to retry: go now
      return
    }
    this.write(bytes)
  }

  /** Input goes out now (not into the queue): attached, and the node there. */
  private get up() { return !this.closed && this.ws?.readyState === WebSocket.OPEN && this.ready && !this.nodeDown }

  /**
   * Input is taken and goes out now: up, and input not refused after a
   * dropped queue (the input box must not take its text then, 第三轮复核 1).
   */
  get canSend() { return this.up && !this.refuses && !this.readOnly }

  /** Input is refused: a queue was dropped, half sent (until unblock) or overflowing (until attached). */
  get refuses() { return this.blocked || this.dropping }

  /** The node went away while this link stayed up: input waits for it (the next attach). */
  nodeOffline() { this.nodeDown = true }

  /** The user has looked at the screen after a half-sent queue was dropped: input is taken again. */
  unblock() { this.blocked = false }

  /** Correlates each bounded chunk; uncertain chunks are never retried automatically. */
  async inputBatch(data: string): Promise<InputResult> {
    const bytes = new TextEncoder().encode(data)
    let acceptedBytes = 0
    const failed = (status: 'rejected' | 'unknown'): InputResult => {
      if (!this.closed && (status === 'unknown' || acceptedBytes)) { this.blocked = true; this.ev.control({ t: 'input_blocked' }) }
      return { status, acceptedBytes }
    }
    for (let off = 0; off < bytes.length; off += 65536) {
      if (!this.canSend || !this.inputAck || this.pending.length) return failed('rejected')
      const piece = bytes.subarray(off, off + 65536)
      const code = await new Promise<string>(resolve => {
        const id = ++this.batchID
        const timer = window.setTimeout(() => finish('unknown'), 5000)
        const finish = (code: string) => { clearTimeout(timer); this.batches.delete(id); resolve(code) }
        this.batches.set(id, finish)
        let encoded = ''
        for (const b of piece) encoded += String.fromCharCode(b)
        try { this.send({ t: 'input_batch', id, data: btoa(encoded) }) } catch { finish('unknown') }
      })
      if (code) {
        return failed(code === 'unknown' ? 'unknown' : 'rejected')
      }
      acceptedBytes += piece.length
    }
    return { status: 'accepted', acceptedBytes }
  }

  private unknownBatches() { for (const finish of this.batches.values()) finish('unknown'); this.inputAck = false }

  private write(bytes: Uint8Array) {
    // docs/M1 第 10 节: one input frame carries at most 64 KiB
    for (let off = 0; off < bytes.length; off += 65536) this.ws!.send(bytes.subarray(off, off + 65536))
  }

  /**
   * Forgets all that waits and says so. Half sent (the start of it is on the
   * node's command line), input stops until the user has looked (unblock):
   * the next Enter would submit a torn command. Else it stops until the next
   * attach, so that what follows the dropped part is not sent without it.
   */
  private drop(uncertain = false) {
    const partial = uncertain || (this.sentSome && this.pending.length > 0)
    this.pending = []
    this.pendingBytes = 0
    this.sentSome = false
    clearTimeout(this.pumpTimer)
    this.pumpTimer = undefined
    if (partial) this.blocked = true
    this.drops++
    this.ev.control({ t: 'input_dropped', partial })
  }

  private overflow() {
    const partial = this.sentSome && this.pending.length > 0
    this.drop()
    if (!partial) {
      this.dropping = !this.up
      if (!this.ws) this.poke()
    }
  }

  /** Sends what was typed while the link was down, now that the session is attached. */
  private flush() {
    this.ready = true
    this.nodeDown = false
    this.dropping = false
    if (!this.pending.length) return
    if (Date.now() - this.pending[0].at > maxPendingAge) { this.drop(); return } // the oldest decides: all of it is at least as new
    if (this.pumpTimer !== undefined) return // a pause under way goes on as it was (复核：重挂载掐掉间隔)
    this.pump()
  }

  // Sends the queue in order. The pauses that mattered are kept (up to
  // maxPause): before an Enter that followed a paste (Claude Code would take
  // it as part of the paste) and after an Esc (an Esc followed at once by a
  // key reads as Alt+key) (问题单 1 第 4 条; Composer.svelte).
  private pump() {
    this.pumpTimer = undefined
    // A page frozen in the background runs this late, the link still up: what
    // waits may be past the minute by now (第三轮复核 2).
    if (this.pending.length && Date.now() - this.pending[0].at > maxPendingAge) { this.drop(); return }
    while (this.pending.length) {
      if (!this.up) return // the rest goes on the next attach
      const p = this.pending.shift()!
      this.pendingBytes -= p.bytes.length
      this.write(p.bytes)
      this.sentSome = true
      const next = this.pending[0]
      if (!next) break
      const pause = Math.min(maxPause, Math.max(0, next.at - p.at))
      const matters = (next.bytes[0] === 0x0d || next.bytes[0] === 0x0a) || (p.bytes.length === 1 && p.bytes[0] === 0x1b)
      if (matters && pause > 0) { this.pumpTimer = window.setTimeout(() => this.pump(), pause); return }
    }
    this.sentSome = false
  }

  private send(m: Ctl) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(m))
  }

  private scheduleReconnect() {
    if (this.closed) return
    const delay = Math.min(15000, 1000 * 2 ** this.attempt++)
    this.timer = window.setTimeout(() => this.connect(), delay)
  }

  /** Reconnect now (network came back, page returned to the foreground). */
  poke() {
    if (this.closed || this.ws) return
    clearTimeout(this.timer)
    this.connect()
  }

  /**
   * Drops the connection but keeps `have`, so that a later poke() resumes
   * instead of replaying: a background tab beyond the mount limit (docs/M9 第 3 节).
   */
  detach() {
    this.unknownBatches()
    clearTimeout(this.timer)
    const ws = this.ws
    this.ws = null
    this.ready = false
    if (ws) {
      ws.onclose = null
      ws.close()
      this.ev.state('detached')
    }
  }

  get connected() { return this.ws?.readyState === WebSocket.OPEN }

  close() {
    this.unknownBatches()
    this.closed = true
    clearTimeout(this.timer)
    clearTimeout(this.pumpTimer)
    this.ws?.close()
  }
}
