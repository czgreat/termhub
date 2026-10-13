// Folders of a node. Windows nodes give C:\... paths, Linux nodes /home/...
// paths; nothing else tells them apart in the browser, and nothing else needs
// to. "//" opens a Windows network path written with slashes.

export const isPosix = (p: string) => p.startsWith('/') && !p.startsWith('//')

export const sepOf = (p: string) => isPosix(p) ? '/' : '\\'

/** Resolve ".", ".." and doubled or trailing slashes, as Go's path.Clean. */
export function posixClean(p: string): string {
  const out: string[] = []
  for (const s of p.split('/')) {
    if (s === '' || s === '.') continue
    if (s === '..') out.pop()
    else out.push(s)
  }
  return '/' + out.join('/')
}

/** The last name of a path, either separator. */
export const baseName = (p: string) => p.replace(/[\\/]+$/, '').split(/[\\/]/).pop() || p

export const joinPath = (dir: string, name: string) => dir.replace(/[\\/]+$/, '') + sepOf(dir) + name

/** Breadcrumbs of a listed folder: C:\ › Users › me, or / › home › me. */
export function crumbsOf(path: string): { name: string; path: string }[] {
  if (isPosix(path)) {
    const parts = path.split('/').filter(Boolean)
    return [{ name: '/', path: '/' }, ...parts.map((p, i) => ({ name: p, path: '/' + parts.slice(0, i + 1).join('/') }))]
  }
  const parts = path.split('\\').filter(Boolean)
  return parts.map((p, i) => ({ name: i === 0 ? p + '\\' : p, path: parts.slice(0, i + 1).join('\\') + (i === 0 ? '\\' : '') }))
}
