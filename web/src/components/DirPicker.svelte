<script lang="ts">
  // Folder browser and "new folder" (docs/M4 第 4 节, M10 第 2 节).
  import { onMount } from 'svelte'
  import { get, post, type Drive, type Listing } from '../lib/api'

  let { nodeId, start, onpick, onclose }: { nodeId: number; start: string; onpick: (p: string) => void; onclose: () => void } = $props()
  let drives = $state<Drive[]>([]), desktop = $state('') // desktop: "" from a node older than the 桌面 button
  let listing = $state<Listing | null>(null)
  let error = $state(''), newName = $state(''), creating = $state(false), showHidden = $state(false)

  const base = $derived(`/api/nodes/${nodeId}/fs`)

  async function load(path: string, page = 0) {
    error = ''
    try {
      listing = await get(`${base}/list?path=${encodeURIComponent(path)}&page=${page}&hidden=${showHidden ? 1 : 0}`)
    } catch (e: any) { error = e.message }
  }
  async function loadDrives() {
    try {
      const r = await get(`${base}/drives`)
      drives = r.drives ?? []; desktop = r.desktop ?? ''
    } catch (e: any) { error = e.message }
  }
  async function mkdir() {
    if (!listing || !newName) return
    error = ''
    try {
      const r = await post(`${base}/mkdir`, { parent: listing.path, name: newName })
      newName = ''; creating = false
      await load(r.path)
    } catch (e: any) { error = e.message }
  }
  // Once, on opening: `start` follows the dialog's folder field and `load`
  // reads showHidden, so a tracked effect would jump back to the start folder
  // whenever either changed while the user was browsing elsewhere.
  onMount(() => { loadDrives(); if (start) load(start) })

  const crumbs = $derived.by(() => {
    if (!listing) return []
    const parts = listing.path.split('\\').filter(Boolean)
    return parts.map((p, i) => ({ name: i === 0 ? p + '\\' : p, path: parts.slice(0, i + 1).join('\\') + (i === 0 ? '\\' : '') }))
  })
  const fmt = (n: number) => n < 1024 ? n + ' B' : n < 1048576 ? (n / 1024).toFixed(0) + ' KB' : n < 1073741824 ? (n / 1048576).toFixed(1) + ' MB' : (n / 1073741824).toFixed(1) + ' GB'
</script>

<div class="modal-bg">
  <div class="card modal picker">
    <div class="row">
      <h3 style="margin:0">选择文件夹</h3>
      <label class="row" style="margin:0 0 0 auto"><input type="checkbox" bind:checked={showHidden} onchange={() => listing && load(listing.path)} style="width:auto" /> 显示隐藏</label>
    </div>
    <div class="drives row">
      {#if desktop}<button onclick={() => load(desktop)} title={desktop} data-testid="dir-desktop">🖥 桌面</button>{/if}
      {#each drives as d}
        <button onclick={() => load(d.letter + '\\')} title={d.free !== undefined ? `可用 ${fmt(d.free)} / ${fmt(d.total ?? 0)}` : d.kind}>{d.letter} {d.label ? `(${d.label})` : ''}</button>
      {/each}
    </div>
    {#if listing}
      <div class="crumbs">
        {#each crumbs as c, i}
          {#if i > 0}<span class="muted">›</span>{/if}
          <button class="crumb" onclick={() => load(c.path)}>{c.name}</button>
        {/each}
      </div>
      <div class="list" data-testid="dir-list">
        {#if listing.parent}
          <button class="entry" onclick={() => load(listing!.parent!)}>📁 ..</button>
        {/if}
        {#each listing.entries as e (e.name)}
          <button class="entry" class:muted={!e.dir} disabled={!e.dir} onclick={() => load(listing!.path.replace(/\\$/, '') + '\\' + e.name)}>
            {e.dir ? '📁' : '📄'} {e.name}{e.link ? ' ↗' : ''}
            {#if !e.dir}<span class="muted size">{fmt(e.size)}</span>{/if}
          </button>
        {/each}
        {#if listing.pages > 1}
          <div class="row muted" style="padding:6px">
            第 {listing.page + 1} / {listing.pages} 页，共 {listing.total} 项
            <button disabled={listing.page === 0} onclick={() => load(listing!.path, listing!.page - 1)}>上一页</button>
            <button disabled={listing.page + 1 >= listing.pages} onclick={() => load(listing!.path, listing!.page + 1)}>下一页</button>
          </div>
        {/if}
      </div>
    {:else}
      <p class="muted">{desktop ? '点“桌面”或选一个盘符开始。' : '选一个盘符开始。'}</p>
    {/if}
    {#if error}<p class="err">{error}</p>{/if}
    <div class="row" style="margin-top:10px">
      {#if creating}
        <input bind:value={newName} placeholder="新文件夹名称" onkeydown={e => e.key === 'Enter' && mkdir()} data-testid="new-folder-name" />
        <button class="primary" onclick={mkdir} data-testid="new-folder-ok">创建</button>
        <button onclick={() => creating = false}>取消</button>
      {:else}
        <button onclick={() => creating = true} disabled={!listing} data-testid="new-folder">新建文件夹</button>
        <span style="flex:1"></span>
        <button onclick={onclose}>取消</button>
        <button class="primary" disabled={!listing} onclick={() => onpick(listing!.path)} data-testid="pick-dir">选择当前目录</button>
      {/if}
    </div>
  </div>
</div>

<style>
  .picker { width: min(680px, 94vw); }
  .drives { flex-wrap: wrap; margin: 10px 0; }
  .crumbs { display: flex; flex-wrap: wrap; gap: 4px; align-items: center; margin-bottom: 6px; }
  .crumb { background: none; border: none; padding: 2px 4px; color: var(--accent); }
  .list { max-height: 45vh; overflow-y: auto; border: 1px solid var(--line); border-radius: 6px; }
  .entry { display: flex; width: 100%; text-align: left; background: none; border: none; border-bottom: 1px solid var(--line); border-radius: 0; padding: 6px 10px; }
  .entry:hover:not(:disabled) { background: var(--panel); }
  .size { margin-left: auto; font-size: 12px; }
</style>
