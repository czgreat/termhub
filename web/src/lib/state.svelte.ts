// Application-wide state: who is logged in, what they can see, the panes and
// tabs, and the /ws/events channel that keeps lists fresh (docs/M1 5.3,
// M9 第 3 节).
import { get, setCSRF, onUnauthorized, ApiError, type Me, type Node, type Profile, type Session } from './api'
import type { Menu } from './menu'
import type { Usage } from './usage'

export type Pane = { tabs: string[]; active: string }
export type Split = 'none' | 'row' | 'col'
export type Mod = 'off' | 'once' | 'locked' // a sticky modifier key on the key bar (docs/M11 第 6 节)

export const app = $state({
  me: null as Me | null,
  listsUser: 0, // user whose latest list snapshot has actually been applied
  nodes: [] as Node[],
  profiles: [] as Profile[],
  sessions: [] as Session[],
  panes: [{ tabs: [], active: '' }] as Pane[], // one pane, or two when split
  split: 'none' as Split,
  ratio: 0.5, // the first pane's share of the split
  focus: 0, // the pane that keyboard shortcuts and the sidebar act on
  // Sessions that ended while their tab was open: the tab stays until the
  // user closes it (docs/M9 第 8 节), so their last known row is kept here.
  ended: {} as Record<string, Session>,
  marks: {} as Record<string, 'output' | 'bell'>, // background tabs with news
  done: {} as Record<string, true>, // sessions that stopped working while out of view (状态点)
  usage: {} as Record<string, Usage>, // each AI session's dollar figure and context share (lib/usage.ts)
  lastSeen: {} as Record<string, number>, // when each tab was last in view
  mountLimit: 8, // sessions kept attached at once (docs/M9 第 3 节; 2 on phones, M11 第 3 节)
  authExpired: false,
  offline: false, // the Hub could not be reached at all (not "not logged in")
  toast: '' as string,
  // phone and touch (docs/M11)
  mobile: false, // narrow screen: single pane, drawer, top bar
  touch: false, // coarse pointer: show the key bar (tablets too)
  drawer: false,
  inputMode: 'direct' as 'direct' | 'box',
  sending: {} as Record<string, boolean>,
  recovery: {} as Record<string, string>, // uncertain sends, never automatically resent
  drafts: {} as Record<string, string>, // the input box's text, per session
  keymod: { ctrl: 'off' as Mod, alt: 'off' as Mod },
  linkState: {} as Record<string, string>, // each terminal's connection state, for the top bar dot
  menus: {} as Record<string, Menu | null>, // a menu seen on each terminal's screen (phone option cards)
  updateReady: false, // a new version of the shell is installed and waiting
  applyUpdate: null as null | (() => void),
})

// Who the page is for. Another user logging in without a reload (the login
// ran out, someone else signs in) must not see what the page still holds of
// the one before: lists, tabs, drafts, history (问题单 1 第 1 条).
let uid = 0
const userResets: (() => void)[] = []
/** Registers what to forget when another user logs in. */
export const onUserChange = (f: () => void) => { userResets.push(f) }

function switchUser(id: number) {
  if (uid) {
    app.listsUser = 0
    app.nodes = []; app.profiles = []; app.sessions = []
    app.panes = [{ tabs: [], active: '' }]; app.split = 'none'; app.focus = 0
    app.ended = {}; app.marks = {}; app.done = {}; app.usage = {}; app.lastSeen = {}; app.menus = {}; app.linkState = {}
    wasBusy = {}
    app.drafts = {}; app.recovery = {}; app.sending = {}
    events?.close(); events = null
    for (const f of userResets) f()
  }
  uid = id
  loadDrafts()
}

export async function loadMe() {
  try {
    const me = await get<Me>('/api/me')
    setCSRF(me.csrf)
    if (me.user.id !== uid) switchUser(me.user.id)
    app.me = me
    app.authExpired = false
    app.offline = false
    return me
  } catch (e) {
    // A 5xx is the proxy while the Hub restarts (a deploy): "cannot reach",
    // retried, not "logged out" (PWA 复核).
    if (!(e instanceof ApiError) || e.status >= 500) { app.offline = true; return null }
    if (app.me) app.authExpired = true
    app.me = null
    app.offline = false
    return null
  }
}

onUnauthorized(() => { if (app.me) { app.authExpired = true; app.me = null } })

// --- device -----------------------------------------------------------------

const modeKey = 'termhub.inputMode'

/** Reads the screen and pointer once and follows their changes. */
export function initDevice() {
  // A phone is a narrow screen whose main pointer is a finger. A desktop
  // window dragged narrow (half-screen snap, a docked devtools) keeps the
  // desktop layout: switching would tear down every terminal (review 09-23).
  // A phone turned sideways is 800-930 px wide but short: still a phone
  // (PWA 复核: landscape tore every terminal down into the desktop layout).
  const narrow = matchMedia('(max-width: 768px)')
  const short = matchMedia('(max-height: 500px)')
  const coarse = matchMedia('(pointer: coarse)')
  const noHover = matchMedia('(hover: none)')
  const apply = () => {
    app.mobile = (narrow.matches || short.matches) && (coarse.matches || noHover.matches)
    app.touch = coarse.matches || app.mobile
    if (app.mobile) app.drawer = false
  }
  apply()
  narrow.addEventListener('change', apply)
  short.addEventListener('change', apply)
  coarse.addEventListener('change', apply)
  noHover.addEventListener('change', apply)
  let saved: string | null = null
  try { saved = localStorage.getItem(modeKey) } catch { /* ignore */ }
  // First time on a phone: the input box, which copes with every input method (docs/M11 第 5 节).
  app.inputMode = saved === 'box' || saved === 'direct' ? saved : app.mobile ? 'box' : 'direct'
}

// The input box's drafts outlive a reload, an update or the phone closing the
// app (PWA 复核: Android's back button, "点击刷新"), kept per user.
const draftsKey = () => 'termhub.drafts.' + uid
let draftTimer: number | undefined
export function saveRecovery() {
  try { localStorage.setItem('termhub.recovery.' + uid, JSON.stringify(app.recovery)) } catch { /* ignore */ }
}
function loadDrafts() {
  try {
    Object.assign(app.recovery, JSON.parse(localStorage.getItem('termhub.recovery.' + uid) || '{}'))
    const legacy = localStorage.getItem('termhub.drafts') // before they were per user: the first user to log in
    if (legacy !== null) { localStorage.removeItem('termhub.drafts'); if (localStorage.getItem(draftsKey()) === null) localStorage.setItem(draftsKey(), legacy) }
    Object.assign(app.drafts, JSON.parse(localStorage.getItem(draftsKey()) || '{}'))
  } catch { /* ignore */ }
}
export function saveDrafts(immediate = false) {
  clearTimeout(draftTimer)
  const key = draftsKey() // the user of the typing, not of whenever the timer fires
  const write = () => {
    if (key !== draftsKey()) return
    const keep = Object.fromEntries(Object.entries(app.drafts).filter(([, v]) => v))
    try { localStorage.setItem(key, JSON.stringify(keep)) } catch { /* ignore */ }
  }
  if (immediate) write(); else draftTimer = window.setTimeout(write, 300)
}

export function setInputMode(m: 'direct' | 'box') {
  app.inputMode = m
  try { localStorage.setItem(modeKey, m) } catch { /* ignore */ }
}

/** Presses a sticky modifier: off → once → locked → off. */
export function tapMod(k: 'ctrl' | 'alt') {
  const v = app.keymod[k]
  app.keymod[k] = v === 'off' ? 'once' : v === 'once' ? 'locked' : 'off'
}

/**
 * Applies the sticky Ctrl/Alt to one key's bytes (a typed character or a key
 * bar sequence) and releases the one-shot ones.
 */
export function applyMods(data: string): string {
  const ctrl = app.keymod.ctrl !== 'off', alt = app.keymod.alt !== 'off'
  if (!ctrl && !alt) return data
  let out = data
  const arrow = /^\x1b(?:\[|O)([ABCDHF])$/.exec(data)
  if (arrow) {
    const mod = 1 + (alt ? 2 : 0) + (ctrl ? 4 : 0)
    out = `\x1b[1;${mod}${arrow[1]}`
  } else if (data.length === 1) {
    const c = data.charCodeAt(0)
    if (ctrl) {
      if (c >= 0x40 && c <= 0x7f) out = String.fromCharCode(c & 0x1f)
      else if (data === ' ') out = '\x00'
    }
    if (alt) out = '\x1b' + out
  }
  if (app.keymod.ctrl === 'once') app.keymod.ctrl = 'off'
  if (app.keymod.alt === 'once') app.keymod.alt = 'off'
  return out
}

export const allOpen = () => app.panes.flatMap(p => p.tabs)
export const isActive = (sid: string) => app.panes.some(p => p.active === sid)
export const focusedSid = () => app.panes[app.focus]?.active ?? ''
export const otherPane = () => (app.split === 'none' || app.panes.length < 2 ? -1 : 1 - app.focus)

let listRequest = 0
export async function refreshLists() {
  if (!app.me || app.me.pending.length) return
  const asked = uid
  const request = ++listRequest
  const [n, p, s] = await Promise.all([get('/api/nodes'), get('/api/profiles'), get('/api/sessions')])
  // Another user logged in meanwhile: this answer (an administrator's profiles
  // carry their environment) is not theirs (第二轮复核 1).
  if (uid !== asked) return
  // A stale answer still shows who was working: with titles changing faster
  // than the lists come back, every answer but the last may be stale (状态点复核 3).
  if (request !== listRequest) { for (const row of s.sessions ?? []) if (busyTitle(row.title)) wasBusy[row.sid] = true; return }
  const before = app.sessions
  app.nodes = n.nodes ?? []
  app.profiles = p.profiles ?? []
  app.sessions = s.sessions ?? []
  for (const running of app.sessions) delete app.ended[running.sid]
  noteBusy()
  // a new AI session gets its figures soon, not at the next 30-second round
  if (Date.now() - usageAt > 10_000 && app.sessions.some(s => (s.kind === 'claude' || s.kind === 'codex') && !app.usage[s.sid])) refreshUsage()
  for (const sid of allOpen()) {
    if (app.sessions.some(s => s.sid === sid) || app.ended[sid]) continue
    const last = before.find(s => s.sid === sid)
    if (last) app.ended[sid] = { ...last, ended_at: last.ended_at || Math.floor(Date.now() / 1000) }
    else closeTab(sid) // never seen it running: nothing to show
  }
  app.listsUser = asked
}
;(globalThis as any).__thRefresh = refreshLists // for tests: what a sessions_changed event does

/** The row for a tab's session: running, or the last one seen before it ended. */
export const sessionOf = (sid: string) => app.sessions.find(s => s.sid === sid) ?? app.ended[sid]

// G4: keep saved text reachable even after its session/tab disappears.
export const savedInputSids = () => [...new Set([...Object.keys(app.recovery), ...Object.keys(app.drafts)])]
  .filter(sid => app.recovery[sid] || app.drafts[sid])

export function toast(msg: string) {
  app.toast = msg
  setTimeout(() => { if (app.toast === msg) app.toast = '' }, 4000)
}

let events: WebSocket | null = null
export function connectEvents() {
  if (events) return
  const url = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ws/events'
  const ws = new WebSocket(url)
  events = ws
  // A link that silently died (Wi-Fi to mobile data) still looks open: with
  // no answer to two pings it is closed and made again (PWA 复核).
  let heard = Date.now()
  const ping = setInterval(() => {
    if (ws.readyState !== WebSocket.OPEN) return
    if (Date.now() - heard > 50000) { ws.close(); return }
    ws.send(JSON.stringify({ t: 'ping' }))
  }, 20000)
  ws.onopen = () => { heard = Date.now(); refreshLists() } // anything that changed while we were away
  ws.onmessage = e => {
    heard = Date.now()
    const m = JSON.parse(e.data)
    switch (m.t) {
      case 'sessions_changed': case 'node_status': case 'session_exited': refreshLists(); break
      case 'auth_expired': app.authExpired = true; app.me = null; break
    }
  }
  ws.onclose = () => {
    clearInterval(ping)
    if (events === ws) events = null
    // the login may be what went away: ask before trying again
    if (app.me && document.visibilityState === 'visible') setTimeout(() => loadMe().then(me => { if (me) connectEvents() }), 3000)
  }
}

// In the background for more than 30 s the page lets go of its connections;
// back in the foreground it reconnects at once (docs/M11 第 8 节). The
// terminals do the same for their own links.
export const backgroundGrace = 30_000
let bgTimer: number | undefined, hiddenAt = 0
/** How long the page was last in the background, in ms (0 while in front). */
export const hiddenFor = () => (hiddenAt ? Date.now() - hiddenAt : 0)
export function watchVisibility() {
  window.addEventListener('focus', clearWatched)
  document.addEventListener('visibilitychange', () => {
    clearTimeout(bgTimer)
    clearWatched()
    if (document.visibilityState === 'hidden') {
      hiddenAt = Date.now()
      if (app.mobile) bgTimer = window.setTimeout(() => events?.close(), backgroundGrace)
      return
    }
    // iOS freezes timers in the background, so the 30 s timer may never have
    // fired: judge by the clock. A long absence also checks the login first.
    const away = Date.now() - hiddenAt
    hiddenAt = 0
    if (!app.me) return
    if (away > backgroundGrace) {
      events?.close()
      events = null
      loadMe().then(me => { if (me) connectEvents() })
    } else connectEvents()
  })
}

// --- what each AI session has cost ------------------------------------------
// Asked every 30 seconds while the page is in view; the Hub keeps the figures
// 15 seconds and the node reads only what the history file grew by.

let usageRequest = 0, usageAt = 0
export async function refreshUsage() {
  if (!app.me || app.me.pending.length || typeof document === 'undefined' || document.visibilityState !== 'visible') return
  if (!app.sessions.some(s => s.kind === 'claude' || s.kind === 'codex')) { app.usage = {}; return }
  const asked = uid, request = ++usageRequest
  usageAt = Date.now()
  try {
    const r = await get('/api/sessions/usage')
    if (uid === asked && request === usageRequest) app.usage = r.sessions ?? {}
  } catch { /* the figures stay as they were; the next round tries again */ }
}

export function watchUsage() {
  refreshUsage()
  setInterval(refreshUsage, 30_000)
  document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible') refreshUsage() })
}

// --- what each session is doing --------------------------------------------
// With several sessions open the owner could not tell which had finished
// (主人 2026-09-25). Claude Code starts the terminal title with a spinner
// while it works (◐◓◑◒, braille dots in some versions) and with ✳ when it
// waits; the Hub passes titles on for every session, open in a tab or not.
// A CLI that writes no such title gets no dot, only the old 有新输出 mark.

const spinning = /^\s*[◐-◓⠀-⣿]/
const glyph = /^\s*[◐-◓⠀-⣿✳]\s*/
export const busyTitle = (t?: string) => !!t && spinning.test(t)
/** A title without the CLI's status glyph, which the dot shows instead. */
export const plainTitle = (t?: string) => (t ?? '').replace(glyph, '')

let wasBusy: Record<string, boolean> = {}

/** Whether the owner is looking at this session right now. */
function inView(sid: string) {
  if (typeof document === 'undefined' || document.visibilityState !== 'visible') return false
  if (typeof document.hasFocus === 'function' && !document.hasFocus()) return false
  return app.mobile ? app.panes[0]?.active === sid && !app.drawer : app.panes.some(p => p.active === sid)
}

/** A session that stopped working unseen is 做完了 until it is looked at. */
function noteBusy() {
  const now: Record<string, boolean> = {}
  for (const s of app.sessions) {
    const busy = busyTitle(s.title)
    if (busy) delete app.done[s.sid]
    else if (wasBusy[s.sid] && !inView(s.sid)) app.done[s.sid] = true
    now[s.sid] = busy
  }
  for (const sid in app.done) if (!(sid in now)) delete app.done[sid]
  wasBusy = now
}

/** Coming back to the window counts as seeing the sessions in view. */
function clearWatched() {
  for (const sid in app.done) if (inView(sid)) delete app.done[sid]
}

export type Status = 'ask' | 'done' | 'busy' | ''
/** ask: an option menu waits on screen; done: finished, not seen yet; busy: working. */
export function statusOf(sid: string): Status {
  const s = app.sessions.find(s => s.sid === sid)
  if (!s || app.ended[sid]) return ''
  if (app.menus[sid]) return 'ask'
  if (busyTitle(s.title)) return 'busy'
  return app.done[sid] ? 'done' : ''
}

// --- panes and tabs -------------------------------------------------------

function touch(sid: string) {
  app.lastSeen[sid] = Date.now()
  delete app.marks[sid]
  delete app.done[sid]
}

/**
 * Shows a session in a pane, opening a tab if needed: where it is already
 * open, else an empty pane (the one just created by a split), else the
 * focused pane.
 */
export function openSession(sid: string, pane = -1) {
  const already = app.panes.findIndex(p => p.tabs.includes(sid))
  const empty = app.panes.findIndex(p => p.tabs.length === 0)
  if (already >= 0) pane = already
  else if (pane < 0) pane = empty >= 0 ? empty : app.focus
  const p = app.panes[pane] ?? app.panes[0]
  if (!p.tabs.includes(sid)) p.tabs.push(sid)
  p.active = sid
  app.focus = app.panes.indexOf(p)
  app.drawer = false
  touch(sid)
  saveLayout()
}

export function activate(sid: string, pane: number) {
  const p = app.panes[pane]
  if (!p || !p.tabs.includes(sid)) return
  p.active = sid
  app.focus = pane
  touch(sid)
  saveLayout()
}

export function closeTab(sid: string) {
  for (const p of app.panes) {
    const i = p.tabs.indexOf(sid)
    if (i < 0) continue
    p.tabs.splice(i, 1)
    if (p.active === sid) {
      p.active = p.tabs[Math.min(i, p.tabs.length - 1)] ?? ''
      if (p.active) touch(p.active) // the tab that takes its place is now in view (状态点复核 4)
    }
  }
  delete app.ended[sid]
  delete app.marks[sid]
  saveLayout()
}

/** Replaces one tab's session by another (a session reopened with the same settings). */
export function replaceTab(oldSid: string, newSid: string) {
  for (const p of app.panes) {
    p.tabs = p.tabs.map(s => (s === oldSid ? newSid : s))
    if (p.active === oldSid) p.active = newSid
  }
  delete app.ended[oldSid]
  touch(newSid)
  saveLayout()
}

/** Cycles the focused pane's active tab by `step` (+1 next, -1 previous). */
export function cycleTab(step: number) {
  const p = app.panes[app.focus]
  if (!p || p.tabs.length < 2) return
  const i = p.tabs.indexOf(p.active)
  activate(p.tabs[(i + step + p.tabs.length) % p.tabs.length], app.focus)
}

/** Splits into two panes ('row' side by side, 'col' stacked) or merges back to one. */
export function setSplit(mode: Split) {
  if (mode === 'none') {
    if (app.panes.length > 1) {
      const [a, b] = app.panes
      for (const sid of b.tabs) if (!a.tabs.includes(sid)) a.tabs.push(sid)
      if (!a.active) a.active = b.active
      if (a.active) touch(a.active)
      app.panes = [a]
    }
    app.focus = 0
  } else if (app.panes.length < 2) {
    app.panes.push({ tabs: [], active: '' })
    app.focus = 1 // the new, empty pane is what the user wants to fill next
  }
  app.split = mode
  saveLayout()
}

export function focusPane(i: number) {
  if (i >= 0 && i < app.panes.length) app.focus = i
}

/**
 * Which tabs keep their session attached: every pane's active tab, then the
 * most recently viewed background tabs up to the limit. The others are
 * detached and remember `have`, so switching back resumes (docs/M9 第 3 节).
 */
export function mountedSet(): Set<string> {
  const set = new Set<string>()
  for (const p of app.panes) if (p.active) set.add(p.active)
  const rest = allOpen().filter(sid => !set.has(sid)).sort((a, b) => (app.lastSeen[b] ?? 0) - (app.lastSeen[a] ?? 0))
  const limit = app.mobile ? 2 : app.mountLimit
  for (const sid of rest) {
    if (set.size >= limit) break
    set.add(sid)
  }
  return set
}

// --- layout persistence (browser local; no terminal content, no secrets) ---

const layoutKey = () => 'termhub.layout.' + uid

export function saveLayout() {
  if (!uid) return
  try {
    localStorage.setItem(layoutKey(), JSON.stringify({ panes: app.panes.map(p => ({ tabs: [...p.tabs], active: p.active })), split: app.split, ratio: app.ratio, focus: app.focus }))
  } catch { /* private mode or storage blocked: the layout is just not remembered */ }
}

/** Restores the saved layout, keeping only tabs whose sessions still run. */
export function restoreLayout() {
  let saved: any
  try {
    // before layouts were per user: the first user to log in takes it over, once
    const legacy = localStorage.getItem('termhub.layout')
    if (legacy !== null) { localStorage.removeItem('termhub.layout'); if (localStorage.getItem(layoutKey()) === null) localStorage.setItem(layoutKey(), legacy) }
    saved = JSON.parse(localStorage.getItem(layoutKey()) ?? 'null')
  } catch { return }
  if (!saved || !Array.isArray(saved.panes)) return
  const running = new Set(app.sessions.map(s => s.sid))
  const panes: Pane[] = saved.panes.slice(0, 2).map((p: any) => {
    const tabs: string[] = (Array.isArray(p.tabs) ? p.tabs : []).filter((s: unknown) => typeof s === 'string' && running.has(s))
    const active = tabs.includes(p.active) ? p.active : tabs[0] ?? ''
    return { tabs, active }
  })
  if (panes.length === 0) return
  const split: Split = !app.mobile && (saved.split === 'row' || saved.split === 'col') ? saved.split : 'none'
  app.panes = split === 'none' ? [panes[0]] : panes.length === 2 ? panes : [panes[0], { tabs: [], active: '' }]
  if (split === 'none' && panes[1]) for (const sid of panes[1].tabs) if (!app.panes[0].tabs.includes(sid)) app.panes[0].tabs.push(sid)
  app.split = split
  app.ratio = typeof saved.ratio === 'number' && saved.ratio > 0.1 && saved.ratio < 0.9 ? saved.ratio : 0.5
  app.focus = saved.focus === 1 && app.panes.length > 1 ? 1 : 0
  for (const p of app.panes) if (p.active) touch(p.active)
}
