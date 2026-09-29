// Touch on the terminal (docs/M11 第 7 节). xterm.js 6.0 ships no touch
// handling of its own: the gesture class it carries is never attached, so a
// finger on the screen neither scrolls nor selects. This module adds both:
//
// - a drag scrolls the history with inertia. On the alternate screen (a
//   full-screen program) it is the program's to scroll: a program that
//   asked for mouse reports (Claude Code's full-screen view does: ?1000,
//   ?1006) gets mouse-wheel reports, which xterm encodes as the program
//   asked; others get arrow keys, like xterm's wheel — but never an AI CLI,
//   which reads ↑ as "the previous prompt": a swipe filled Claude Code's
//   input with old messages and 发送 then sent them (主人 2026-09-26);
// - a long press selects the word under the finger and shows two handles,
//   dragging a handle changes that end of the selection; the page decides
//   what to do with the text (copy, send to the input box).
import type { Terminal } from '@xterm/xterm'

export interface Cell { col: number; row: number } // row: absolute buffer row
export interface SelectionState { start: Cell; end: Cell }

export interface TouchOptions {
  term: Terminal
  host: HTMLElement
  /** keys for the program when it owns the screen (alternate buffer) */
  send: (keys: string) => void
  /** whether a full-screen program without mouse reports may get arrow keys for a drag */
  arrowsOk?: () => boolean
  /** lines a drag scrolled a full-screen program by (up > 0), for the 回到底部 button */
  onAltScroll?: (up: number) => void
  /** called whenever a selection starts, moves or ends (null = gone) */
  onSelection: (s: SelectionState | null) => void
}

const longPressMs = 500, slop = 8

export function cellSize(term: Terminal): { w: number; h: number } {
  const d = (term as any)._core?._renderService?.dimensions?.css?.cell
  if (d?.width && d?.height) return { w: d.width, h: d.height }
  const screen = term.element?.querySelector('.xterm-screen') as HTMLElement | null
  if (screen && term.cols && term.rows) return { w: screen.clientWidth / term.cols, h: screen.clientHeight / term.rows }
  return { w: 8, h: 17 }
}

export function screenOrigin(term: Terminal): DOMRect {
  const screen = term.element?.querySelector('.xterm-screen') as HTMLElement | null
  return screen ? screen.getBoundingClientRect() : new DOMRect()
}

/** The buffer cell under a point on the page (clamped to the screen). */
export function cellAt(term: Terminal, x: number, y: number): Cell {
  const o = screenOrigin(term), c = cellSize(term)
  const col = Math.max(0, Math.min(term.cols - 1, Math.floor((x - o.left) / c.w)))
  const line = Math.max(0, Math.min(term.rows - 1, Math.floor((y - o.top) / c.h)))
  return { col, row: term.buffer.active.viewportY + line }
}

/** Position of a cell's top-left on the screen, or null when scrolled out of view. */
export function cellPos(term: Terminal, cell: Cell): { x: number; y: number } | null {
  const line = cell.row - term.buffer.active.viewportY
  if (line < 0 || line >= term.rows) return null
  const c = cellSize(term)
  return { x: cell.col * c.w, y: line * c.h }
}

function before(a: Cell, b: Cell) { return a.row < b.row || (a.row === b.row && a.col <= b.col) }

/** Applies a selection between two cells (either order), inclusive. */
export function applySelection(term: Terminal, s: SelectionState) {
  const [a, b] = before(s.start, s.end) ? [s.start, s.end] : [s.end, s.start]
  const length = (b.row - a.row) * term.cols + (b.col - a.col) + 1
  term.select(a.col, a.row, length)
}

/** The word around a cell, for the first selection of a long press. */
export function wordAt(term: Terminal, cell: Cell): SelectionState {
  const text = term.buffer.active.getLine(cell.row)?.translateToString() ?? ''
  const isWord = (ch: string) => ch !== '' && !/[\s│┃|]/.test(ch)
  let lo = cell.col, hi = cell.col
  if (!isWord(text[cell.col] ?? '')) return { start: cell, end: cell }
  while (lo > 0 && isWord(text[lo - 1])) lo--
  while (hi < text.length - 1 && isWord(text[hi + 1])) hi++
  return { start: { col: lo, row: cell.row }, end: { col: hi, row: cell.row } }
}

export interface TouchHandle {
  detach(): void; clear(): void; current(): SelectionState | null
  /** n wheel notches down in the middle of the screen: a full-screen program back towards its latest output */
  wheelDown(n: number): void
}

export function attachTouch(o: TouchOptions): TouchHandle {
  const { term, host } = o
  let startY = 0, startX = 0, lastY = 0, lastT = 0, moved = false, pressTimer: number | undefined
  let velocity = 0, inertia: number | undefined, remainder = 0
  let selection: SelectionState | null = null
  let dragging: 'start' | 'end' | null = null

  const arrows = (n: number) => {
    const app = term.modes.applicationCursorKeysMode
    const key = (n < 0 ? '\x1b[A' : '\x1b[B').replace('[', app ? 'O' : '[')
    o.send(key.repeat(Math.abs(n)))
  }
  // One wheel notch per line, at the finger: xterm reports it to the program
  // in the protocol and encoding the program enabled.
  const wheel = (n: number, x = startX, y = startY, max = 20) => {
    const target = term.element?.querySelector('.xterm-screen') ?? term.element
    if (!target) return
    for (let i = 0; i < Math.min(Math.abs(n), max); i++) {
      target.dispatchEvent(new WheelEvent('wheel', { deltaY: n > 0 ? -1 : 1, deltaMode: 1, clientX: x, clientY: y, bubbles: true, cancelable: true }))
    }
  }
  // dy in pixels → whole lines, keeping the fraction for the next move
  function scrollBy(dy: number) {
    remainder += dy / cellSize(term).h
    const lines = Math.trunc(remainder)
    if (!lines) return
    remainder -= lines
    if (term.buffer.active.type !== 'alternate') term.scrollLines(-lines)
    else if (term.modes.mouseTrackingMode !== 'none' && term.modes.mouseTrackingMode !== 'x10') { wheel(lines); o.onAltScroll?.(lines) } // X10 reports no wheel
    else if (o.arrowsOk?.() ?? true) arrows(-lines)
  }
  function stopInertia() { if (inertia) cancelAnimationFrame(inertia); inertia = undefined }
  function glide() {
    if (Math.abs(velocity) < 0.02) { inertia = undefined; return }
    scrollBy(velocity * 16)
    velocity *= 0.94
    inertia = requestAnimationFrame(glide)
  }

  function setSelection(s: SelectionState | null) {
    selection = s
    if (s) applySelection(term, s); else term.clearSelection()
    o.onSelection(s)
  }

  function onStart(e: TouchEvent) {
    if (e.touches.length !== 1) return
    const t = e.touches[0]
    const handle = (t.target as HTMLElement).closest?.('[data-handle]') as HTMLElement | null
    if (handle && selection) { dragging = handle.dataset.handle as 'start' | 'end'; e.preventDefault(); return }
    stopInertia()
    startX = t.clientX; startY = lastY = t.clientY; lastT = e.timeStamp; moved = false; velocity = 0; remainder = 0
    if (selection) setSelection(null) // a tap elsewhere ends the selection
    clearTimeout(pressTimer)
    pressTimer = window.setTimeout(() => {
      pressTimer = undefined
      navigator.vibrate?.(30)
      setSelection(wordAt(term, cellAt(term, startX, startY)))
    }, longPressMs)
  }
  function onMove(e: TouchEvent) {
    if (e.touches.length !== 1) return
    const t = e.touches[0]
    if (dragging && selection) {
      e.preventDefault()
      const cell = cellAt(term, t.clientX, t.clientY)
      setSelection(dragging === 'start' ? { start: cell, end: selection.end } : { start: selection.start, end: cell })
      return
    }
    if (!moved && Math.hypot(t.clientX - startX, t.clientY - startY) < slop) return
    if (!moved) { moved = true; clearTimeout(pressTimer); pressTimer = undefined; if (selection) return }
    if (selection) return // the selection stays; no scrolling under it
    e.preventDefault()
    const dy = t.clientY - lastY, dt = Math.max(1, e.timeStamp - lastT)
    velocity = velocity * 0.3 + (dy / dt) * 0.7
    lastY = t.clientY; lastT = e.timeStamp
    scrollBy(dy)
  }
  function onEnd(e: TouchEvent) {
    clearTimeout(pressTimer); pressTimer = undefined
    if (dragging) { dragging = null; return }
    if (moved && !selection && Math.abs(velocity) > 0.05 && e.timeStamp - lastT < 80) inertia = requestAnimationFrame(glide)
    velocity = 0
  }
  // The browser turns a touch into mouse events afterwards; after a long
  // press xterm's mousedown would drop the selection and the context menu
  // would open. Only those left-behind events are stopped: a plain tap keeps
  // its mousedown (that is how the terminal takes the focus in direct-input
  // mode) and a real mouse is never touched, so on a touch-screen desktop the
  // mouse keeps working while a touch selection is showing.
  let touching = 0
  const guard = (e: Event) => {
    if (!touching) return
    if (selection || e.type === 'contextmenu') { e.preventDefault(); e.stopPropagation() }
  }
  // xterm clearing its selection (a mouse click, Ctrl+C) ends ours as well
  const selSub = term.onSelectionChange(() => { if (selection && !term.hasSelection()) setSelection(null) })
  const touchOn = () => { touching++ }
  const touchOff = () => { setTimeout(() => { touching = Math.max(0, touching - 1) }, 400) }
  host.addEventListener('touchstart', touchOn, { passive: true })
  host.addEventListener('touchend', touchOff, { passive: true })
  host.addEventListener('touchcancel', touchOff, { passive: true })
  host.addEventListener('touchstart', onStart, { passive: false })
  host.addEventListener('touchmove', onMove, { passive: false })
  host.addEventListener('touchend', onEnd)
  host.addEventListener('touchcancel', onEnd)
  host.addEventListener('mousedown', guard, true)
  host.addEventListener('contextmenu', guard, true)
  const scrollSub = term.onScroll(() => { if (selection) o.onSelection(selection) }) // handles follow the view
  return {
    clear: () => setSelection(null),
    current: () => selection,
    wheelDown: n => {
      const r = (term.element?.querySelector('.xterm-screen') ?? term.element)?.getBoundingClientRect()
      if (r) wheel(-n, r.left + r.width / 2, r.top + r.height / 2, 500)
    },
    detach: () => {
      stopInertia(); clearTimeout(pressTimer); scrollSub.dispose(); selSub.dispose()
      for (const [t, f] of [['touchstart', onStart], ['touchmove', onMove], ['touchend', onEnd], ['touchcancel', onEnd], ['touchstart', touchOn], ['touchend', touchOff], ['touchcancel', touchOff]] as const) host.removeEventListener(t, f as any)
      host.removeEventListener('mousedown', guard, true); host.removeEventListener('contextmenu', guard, true)
    },
  }
}
