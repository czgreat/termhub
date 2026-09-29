// Menus on the screen (docs/M11 第 6 节 "选项条"): Claude Code and Codex draw
// their choices as numbered lines, as a list with a cursor mark, or as a list
// of checkboxes. Read from the terminal's buffer, they become tappable cards
// on a phone; a tap sends the keys a person would press.

export type MenuKind = 'numbered' | 'cursor' | 'multi'
export interface MenuItem { label: string; number?: number; cursor: boolean; checked?: boolean }
export interface MenuExtra { label: string; keys: string }
export interface Menu {
  kind: MenuKind; items: MenuItem[]; cursor: number; extras?: MenuExtra[]; title?: string
  /** multi-select with a "Submit" row (Claude Code's questions): ↓/↑ presses from the cursor to that row */
  submitOffset?: number
}

const cursorMarks = /^[\s]*[❯›>▶●•■]\s/
const pointerMark = /^[\s]*[❯›>▶]\s/
const prefix = /^[\s]*(?:[❯›>▶]\s*)?/
const numbered = /^[\s]*(?:[❯›>▶]\s*)?(\d{1,2})[.)]\s+(\S.*)$/
const checkbox = /^[\s]*(?:[❯›>▶]\s*)?(\[[ xX✓√×]\]|[☐☑◻◼◯◉])\s+(\S.*)$/
const checkedMark = /[xX✓√×☑◼◉]/
const boxAtStart = /^(\[[ xX✓√×]\]|[☐☑◻◼◯◉])\s*/
const submitRow = /^[\s]*(?:[❯›>▶]\s*)?Submit\s*$/
const chatRow = /^[\s]*(?:[❯›>▶]\s*)?(\d{1,2})[.)]\s+Chat about this/i
const rule = /^[\s]*[─━═]+\s*$/
const menuReach = 8 // rows allowed between a live menu and the last row
const listMark = /^[\s]*[◯○◉●•◦□■☐☑◻◼]\s/ // a row of a list of marked choices

/** Finds a menu in the visible rows (last rows first, since prompts sit at the bottom). */
export function detectMenu(rows: string[]): Menu | null {
  // Trim trailing blank rows; work on a window of the bottom 40 rows.
  let end = rows.length
  while (end > 0 && rows[end - 1].trim() === '') end--
  const win = rows.slice(Math.max(0, end - 40), end)
  // Several menus may still be on the screen; the one nearest the bottom is
  // the live one.
  const blocks = [findMulti(win), findNumbered(win)].filter((f): f is Found => !!f)
  const cur = findCursor(win)
  // a cursor list that lies inside a numbered or checkbox block is that block
  if (cur && !blocks.some(b => cur.start <= b.end && cur.end >= b.start)) blocks.push(cur)
  if (!blocks.length) return null
  blocks.sort((a, b) => b.end - a.end)
  const best = blocks[0]
  // A live prompt sits at the bottom, a hint line or two under it at most.
  // Higher up it is history: Claude Code's full-screen view pins the last
  // message at the top as "> …" with its wrapped lines under it, which read
  // as a pointer and choices (主人 2026-09-26: 一滑动就出选项).
  if (win.length - best.end > menuReach) return null
  best.menu.extras = extrasBelow(win, best.end, best.menu)
  best.menu.title = titleAbove(win, best.start)
  return best.menu
}

interface Found { menu: Menu; start: number; end: number }

// The question above the choices ("Do you want to create a.txt?").
function titleAbove(win: string[], start: number): string | undefined {
  for (let i = start - 1; i >= Math.max(0, start - 3); i--) {
    const t = win[i].trim()
    if (t === '') continue
    return /[?？:：]$/.test(t) ? t : undefined
  }
  return undefined
}

// The hint line under a prompt ("Esc to cancel · Tab to amend", "(shift+tab)")
// names actions that have no number; they become cards too. A question's
// "N. Chat about this" below the rule is a numbered row of its own.
function extrasBelow(win: string[], end: number, menu: Menu): MenuExtra[] {
  const below = win.slice(end, Math.min(win.length, end + 5))
  const text = win.slice(Math.max(0, end - 4), Math.min(win.length, end + 5)).join('\n')
  const out: MenuExtra[] = []
  const chat = below.map(l => chatRow.exec(l)).find(m => m)
  if (chat && !menu.items.some(it => it.number === +chat[1])) out.push({ label: `聊聊这个（${chat[1]}）`, keys: chat[1] })
  if (/Tab to amend|\(tab\)/i.test(text) || (!chat && /Chat about this/i.test(text))) out.push({ label: '补充说明（Tab）', keys: '\t' })
  if (/shift\+tab/i.test(text)) out.push({ label: 'Shift+Tab', keys: '\x1b[Z' })
  if (/Esc to cancel|\(esc\)/i.test(text)) out.push({ label: '取消（Esc）', keys: '\x1b' })
  return out
}

const indentOf = (l: string) => l.length - l.trimStart().length
// where an item's text starts, past the cursor mark: "> 1. Yes" and "  2. No" line up
const textIndent = (l: string) => prefix.exec(l)![0].length

function block<T extends { label: string }>(win: string[], parse: (line: string) => T | null): { start: number; end: number; items: T[] } | null {
  // The last run of at least two consecutive matching lines. A line that
  // starts at or past the items' text is part of the previous item: the tail
  // Claude Code wraps itself on a narrow screen, or a choice's description
  // line under it. A rule, a "Submit" row or a shallower line ends the run.
  let best: { start: number; end: number; items: T[] } | null = null
  let run: T[] = [], start = -1, itemIndent = 0
  const flush = (end: number) => { if (run.length >= 2) best = { start, end, items: run }; run = []; start = -1 }
  win.forEach((line, i) => {
    const it = parse(line)
    if (it) {
      if (run.length === 0) { start = i; itemIndent = textIndent(line) }
      run.push(it)
    } else if (line.trim() === '') {
      if (run.length && i + 1 < win.length && parse(win[i + 1])) return // one blank inside a menu
      flush(i)
    } else if (run.length && indentOf(line) >= itemIndent && !rule.test(line) && !submitRow.test(line)) {
      const last = run[run.length - 1], t = line.trim()
      if (!last.label.endsWith(t)) last.label += ' ' + t // continuation (a description repeating the label is dropped)
    } else flush(i)
  })
  flush(win.length)
  return best
}

function findNumbered(win: string[]): Found | null {
  const b = block<MenuItem>(win, line => {
    const m = numbered.exec(line)
    return m ? { label: m[2].trim(), number: +m[1], cursor: cursorMarks.test(line) } : null
  })
  if (!b) return null
  // numbers must count up from 1
  if (b.items.some((it, i) => it.number !== i + 1)) return null
  let cursor = b.items.findIndex(it => it.cursor)
  // Claude Code's multi-select question: every numbered choice carries a
  // checkbox, a digit toggles it, and a "Submit" row under the list (reached
  // with ↓, then Enter) sends the answer.
  if (b.items.every(it => boxAtStart.test(it.label))) {
    for (const it of b.items) { it.checked = checkedMark.test(boxAtStart.exec(it.label)![1]); it.label = it.label.replace(boxAtStart, '') }
    let submitAt = -1, pos = cursor, row = b.items.length
    for (let i = b.end; i < Math.min(win.length, b.end + 4); i++) {
      const l = win[i]
      if (submitRow.test(l)) { submitAt = row; if (cursorMarks.test(l)) pos = row; row++ }
      else if (chatRow.test(l)) { if (cursorMarks.test(l)) pos = row; row++ }
    }
    const menu: Menu = { kind: 'multi', items: b.items, cursor: cursor < 0 ? 0 : cursor }
    if (submitAt >= 0) menu.submitOffset = submitAt - (pos < 0 ? 0 : pos)
    return { menu, start: b.start, end: b.end }
  }
  return { menu: { kind: 'numbered', items: b.items, cursor }, start: b.start, end: b.end }
}

function findMulti(win: string[]): Found | null {
  const b = block(win, line => {
    const m = checkbox.exec(line)
    return m ? { label: m[2].trim(), cursor: cursorMarks.test(line), checked: /[xX✓☑◼◉]/.test(m[1]) } : null
  })
  if (!b) return null
  const cursor = b.items.findIndex(it => it.cursor)
  return { menu: { kind: 'multi', items: b.items, cursor: cursor < 0 ? 0 : cursor }, start: b.start, end: b.end }
}

function findCursor(win: string[]): Found | null {
  // One line carries the cursor mark; its neighbours with the same indentation
  // and no mark are the other choices.
  // A pointer (❯ › > ▶) wins over a dot near it: in Claude Code's agent
  // switcher "● main" / "◯ opus-reviewer" are radio dots, and the pointer
  // may sit on either line.
  const last = (re: RegExp) => win.map((l, i) => (re.test(l) ? i : -1)).filter(i => i >= 0).pop() ?? -1
  const anyMark = last(cursorMarks), pointed = last(pointerMark)
  const mark = pointed >= 0 && pointed >= anyMark - 6 ? pointerMark : cursorMarks
  const idx = mark === pointerMark ? pointed : anyMark
  if (idx < 0) return null
  const markLine = win[idx]
  const indent = markLine.length - markLine.trimStart().length
  const textOf = (l: string) => l.replace(mark, '').trim()
  const sibling = (l: string) => {
    if (l.trim() === '' || mark.test(l)) return false
    const ind = l.length - l.trimStart().length
    // the other choices sit under the text, i.e. more indented than the mark
    return ind > indent && ind <= indent + 4 && textOf(l).length > 0 && !/^[─━═]+$/.test(l.trim())
  }
  let lo = idx, hi = idx
  while (lo - 1 >= 0 && sibling(win[lo - 1])) lo--
  while (hi + 1 < win.length && sibling(win[hi + 1])) hi++
  if (hi - lo < 1) return null
  // A dot alone is how Claude Code starts every reply and tool call ("● 我确认了…"
  // with its wrapped lines under it): without a pointer, the other rows must be
  // marked choices too, or it is prose (主人 2026-09-26: 手机上把回复认成了选项).
  if (mark !== pointerMark) {
    for (let i = lo; i <= hi; i++) if (i !== idx && !listMark.test(win[i])) return null
  }
  const items: MenuItem[] = []
  for (let i = lo; i <= hi; i++) items.push({ label: textOf(win[i]), cursor: i === idx })
  return { menu: { kind: 'cursor', items, cursor: idx - lo }, start: lo, end: hi + 1 }
}

const arrowKey = (appCursor: boolean, dir: 'A' | 'B') => (appCursor ? '\x1bO' : '\x1b[') + dir
const arrows = (appCursor: boolean, n: number) => arrowKey(appCursor, n < 0 ? 'A' : 'B').repeat(Math.abs(n))

/** The keys to send to pick item i of the menu (arrows are plain, not application mode). */
export function keysToPick(menu: Menu, i: number, appCursor: boolean): string {
  if (menu.items[i].number) return String(menu.items[i].number) // a digit picks, or toggles
  const from = menu.cursor < 0 ? 0 : menu.cursor
  return arrows(appCursor, i - from) + (menu.kind === 'multi' ? ' ' : '\r')
}

/**
 * One key per element: an escape sequence or a single character. Claude Code
 * takes several keys arriving in one burst for a paste and drops the arrows
 * in it (seen with a real CLI, 09-23), so the option bar sends them one at a time.
 */
export function splitKeys(keys: string): string[] {
  return keys.match(/\x1b\[[0-9;]*[A-Za-z~]|\x1bO[A-Za-z]|\x1b.|[\s\S]/g) ?? []
}

/** The keys that submit a multi-select: Enter, after moving to the "Submit" row when there is one. */
export function keysToSubmit(menu: Menu, appCursor: boolean): string {
  return arrows(appCursor, menu.submitOffset ?? 0) + '\r'
}
