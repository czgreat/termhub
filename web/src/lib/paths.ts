// File paths in terminal output (docs/M9 第 8 节): found in a line of text,
// made absolute against the session's folder, and turned into xterm links
// that download the file (a folder comes as a zip, docs/M4 第 6 节). Whether
// the path exists, and whether it may be read, is the node's call.
import type { IBufferRange, ILink, ILinkProvider, Terminal } from '@xterm/xterm'

export interface PathHit { start: number; end: number; text: string } // [start, end) in the line

// Characters that never belong to a path in running text. Spaces end a path
// too: a path with spaces can still be selected by hand and downloaded.
const stop = `\\s"'<>|*?\`，。；：、（）【】《》“”‘’！？`
const seg = `[^${stop}\\\\/:]+`
const absolute = new RegExp(`[A-Za-z]:[\\\\/](?:${seg}[\\\\/]?)*`, 'g')
// Relative: at least one separator (src\\a.ts, ./x, ..\\y), or a bare name
// with an extension that starts with a letter (README.md, main.go).
const relative = new RegExp(`(?:\\.{1,2}[\\\\/])*(?:${seg}[\\\\/])+${seg}|[\\w\\-\\u4e00-\\u9fff]+(?:\\.[\\w\\-]+)*\\.[A-Za-z][A-Za-z0-9]{0,7}`, 'g')

/** Trims what running text puts after a path: punctuation, a line:column suffix. */
function trimEnd(s: string): string {
  let t = s
  for (;;) {
    const u = t.replace(/(?::\d+){1,2}$/, '').replace(/\(\d+(?:,\d+)?\)$/, '').replace(/[.,;:)\]}>]+$/, '')
    if (u === t) return t
    t = u
  }
}

// The patterns backtrack over a long run without separators, so they only
// ever see one whitespace-delimited word of bounded length; a very long
// logical line (base64, minified code) is not searched at all (复核：长行卡页面).
const maxLine = 4096, maxWord = 512

export function findPaths(line: string): PathHit[] {
  const hits: PathHit[] = []
  if (line.length > maxLine) return hits
  const words = /\S+/g
  for (let w; (w = words.exec(line));) {
    const word = w[0], at = w.index
    if (word.length > maxWord || word.includes('://')) continue // too long to be a path, or a URL (the web-links addon's)
    const inWord: PathHit[] = []
    const taken = (a: number, b: number) => inWord.some(h => a < h.end && b > h.start)
    for (const re of [absolute, relative]) {
      re.lastIndex = 0
      for (let m; (m = re.exec(word));) {
        const text = trimEnd(m[0])
        const start = m.index, end = start + text.length
        if (!text || taken(start, end)) continue
        const prev = word[start - 1] ?? ''
        if (re === relative && /[\w\\/@.:~$%-]/.test(prev)) continue // the tail of something else
        if (re === relative && !/[A-Za-z\u4e00-\u9fff]/.test(text)) continue // 1.2.3, 10/20
        if (re === relative && text.length < 4) continue // e.g, i.e
        if (re === absolute && text.length < 4) continue // a bare "C:\"
        inWord.push({ start, end, text })
      }
    }
    for (const h of inWord) hits.push({ start: at + h.start, end: at + h.end, text: h.text })
  }
  return hits.sort((a, b) => a.start - b.start)
}

/** The absolute Windows path for what the terminal showed, given the session's folder. */
export function resolvePath(p: string, cwd: string): string {
  let s = p.replace(/\//g, '\\')
  if (/^[A-Za-z]:\\/.test(s)) return s
  s = s.replace(/^(\.\\)+/, '')
  return cwd ? cwd.replace(/\\+$/, '') + '\\' + s : s
}

/**
 * xterm link provider for paths. A logical line may wrap over several rows;
 * cells, not string indices, give the link's range, so wide (CJK) characters
 * line up.
 */
export function pathLinkProvider(term: Terminal, open: (path: string) => void, hover?: (text: string | null) => void): ILinkProvider {
  return {
    provideLinks(y, callback) {
      const b = term.buffer.active
      let first = y - 1, last = y - 1
      while (first > 0 && b.getLine(first)?.isWrapped) first--
      while (b.getLine(last + 1)?.isWrapped) last++
      let text = ''
      const at: { x: number; y: number }[] = [] // per UTF-16 unit: 1-based cell
      for (let r = first; r <= last; r++) {
        const line = b.getLine(r)
        if (!line) break
        for (let x = 0; x < term.cols; x++) {
          const cell = line.getCell(x)
          if (!cell || cell.getWidth() === 0) continue // second half of a wide character
          const ch = cell.getChars() || ' '
          for (let i = 0; i < ch.length; i++) at.push({ x: x + 1, y: r + 1 })
          text += ch
        }
      }
      const links: ILink[] = []
      for (const h of findPaths(text)) {
        const range: IBufferRange = { start: at[h.start], end: at[h.end - 1] }
        if (!range.start || !range.end || range.end.y < y || range.start.y > y) continue
        links.push({
          range, text: h.text, decorations: { underline: true, pointerCursor: true },
          activate: (_e, t) => open(t),
          hover: () => hover?.(h.text), leave: () => hover?.(null),
        })
      }
      callback(links.length ? links : undefined)
    },
  }
}
