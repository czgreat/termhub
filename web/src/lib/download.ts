// Download a file or folder from a node (docs/M4 第 6 节). The Hub hands out a
// one-time link bound to this login; a folder comes as a zip.
import { get, post, type Entry } from './api'
import { toast } from './state.svelte'

export async function downloadPath(nodeId: number, path: string) {
  let st: Entry
  try {
    st = await get(`/api/nodes/${nodeId}/fs/stat?path=${encodeURIComponent(path)}`)
  } catch (e: any) { toast(`无法下载 ${path}：${e.message}`); return }
  if (st.dir && !confirm(`把文件夹 ${path} 打包成压缩包下载？`)) return
  try {
    const { url } = await post(`/api/nodes/${nodeId}/fs/downloads`, { path, folder: st.dir })
    let href = url
    if ((navigator as any).standalone && !st.dir && st.size < 200 << 20) {
      // An installed iPhone app would navigate itself to the file with no way
      // back, and Safari has not this app's login: fetch it here and save the
      // bytes, which iOS shows in its own viewer with a Done button (PWA 复核).
      toast(`正在取 ${st.name}…`)
      const r = await fetch(url, { credentials: 'same-origin' })
      if (!r.ok) throw new Error('HTTP ' + r.status)
      href = URL.createObjectURL(await r.blob())
      setTimeout(() => URL.revokeObjectURL(href), 60_000)
    }
    const a = document.createElement('a')
    a.href = href; a.download = href === url ? '' : st.name; a.rel = 'noopener'
    document.body.appendChild(a); a.click(); a.remove()
    toast(st.dir ? `开始下载 ${st.name}.zip` : `开始下载 ${st.name}`)
  } catch (e: any) { toast(`无法下载 ${path}：${e.message}`) }
}
