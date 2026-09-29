<script lang="ts">
  // Conversations by machine (docs/M10 第 3.5 节): every machine this user has
  // talked to, most recently used first. Closed, a machine shows its two newest
  // conversations; open, its folders and their conversations. A conversation
  // opens as a read-only record; resuming it is a separate, deliberate step.
  // Folders and conversations can be hidden (and folders renamed) in this
  // user's list only: nothing on the machine is changed or deleted.
  import { onMount, untrack } from 'svelte'
  import { app } from '../lib/state.svelte'
  import { hist, historyProfiles, load, loadAll, loadHidden, setHidden, setFolder, machines, shareGroups, convTitle, ago, fullTime, start,
    type Conv, type ConvRef, type Folder, type HiddenFolder } from '../lib/history.svelte'

  const profiles = $derived(historyProfiles())
  const list = $derived(machines(hist.query))
  const openConvs = $derived(new Set(Object.values(hist.byProfile).flatMap(e => e.data?.running ?? []).map(r => r.conv_id).filter(Boolean)))

  let searching = $state(false)
  let menu = $state('') // the open ⋯ menu: 'f:<folder key>' or 'c:<conversation id>'
  const perFolder = 10
  let more = $state<Record<string, boolean>>({})

  // what is open is remembered on this device; machines start closed, folders open
  const readSet = (k: string) => { try { return new Set<string>(JSON.parse(localStorage.getItem(k) || '[]')) } catch { return new Set<string>() } }
  let openMachines = $state(readSet('th-history-machines-open'))
  let closedFolders = $state(readSet('th-history-folders-closed'))
  function flip(set: Set<string>, key: string, store: string) {
    const next = new Set(set)
    next.has(key) ? next.delete(key) : next.add(key)
    try { localStorage.setItem(store, JSON.stringify([...next])) } catch {}
    return next
  }

  // Loading. The effects track nothing but what they name: load() reads and
  // writes the lists' own state, and tracking that would ask again for ever.
  const ids = $derived(profiles.map(p => p.id).join(','))
  $effect(() => { void ids; untrack(() => loadAll()) })
  // Shown again (a phone opens the drawer, which mounts this anew): a list
  // older than a minute is read again quietly (PWA 复核).
  onMount(() => {
    const old = profiles.some(p => { const e = hist.byProfile[p.id]; return e?.at && Date.now() - e.at > 60_000 })
    if (old) loadAll({ force: true, quiet: true })
    // A tap anywhere outside closes the ⋯ menu. Capture phase: the drawer
    // stops clicks from bubbling, and the tap must not reach a row below
    // the menu either (PWA 复核: 误点到“隐藏”).
    const outside = (e: Event) => {
      if (!menu) return
      const t = e.target as Element | null
      if (t?.closest('.pop, .dots')) return
      menu = ''
      e.stopPropagation(); e.preventDefault()
    }
    document.addEventListener('pointerdown', outside, true)
    return () => document.removeEventListener('pointerdown', outside, true)
  })
  // a node came back after a failed look: once more (bounded by status changes)
  const onlineKey = $derived(app.nodes.filter(n => n.online).map(n => n.id).join(','))
  let wasOnline = new Set<number>()
  $effect(() => {
    const now = new Set(onlineKey ? onlineKey.split(',').map(Number) : [])
    untrack(() => {
      for (const p of historyProfiles()) {
        const e = hist.byProfile[p.id]
        if (e?.error && !e.loading && now.has(p.node_id) && !wasOnline.has(p.node_id)) load(p.id)
      }
      wasOnline = now
    })
  })
  // a session started or ended: the files have probably moved on
  let refreshTimer: number | undefined, lastCount = -1
  $effect(() => {
    const n = app.sessions.length
    untrack(() => {
      const changed = lastCount >= 0 && n !== lastCount
      lastCount = n
      clearTimeout(refreshTimer)
      if (changed) refreshTimer = window.setTimeout(() => loadAll({ force: true, quiet: true }), 2500)
    })
  })

  function openConv(c: ConvRef) {
    hist.view = { profileId: c.profileId, conv: c }
    app.drawer = false
    menu = ''
  }
  const selected = (c: ConvRef) => hist.view?.conv.id === c.id && hist.view?.profileId === c.profileId
  const cmenu = (c: ConvRef) => `c:${c.profileId}:${c.id}`
  function renameFolder(f: Folder) {
    menu = ''
    const orig = f.cwd.replace(/[\\/]+$/, '').split(/[\\/]/).pop() || f.cwd
    const name = prompt(`在列表里显示的名字（只改显示，不改真实文件夹 ${f.cwd}）。留空恢复为“${orig}”。`, f.name)
    if (name !== null) setFolder(f.profileId, f.cwd, { name: name.trim() })
  }
  function hideFolder(f: Folder) {
    menu = ''
    if (confirm(`在列表里隐藏文件夹“${f.name}”和它的 ${f.total} 段对话？\n\n只是不显示，文件和对话都不会删除，可以在“已隐藏”里恢复。`)) setFolder(f.profileId, f.cwd, { hidden: true })
  }
  async function hideConv(c: ConvRef) {
    menu = ''
    if (await setHidden(c.profileId, c, true) && selected(c)) hist.view = null
  }
  const profileName = (id: number) => app.profiles.find(p => p.id === id)?.name ?? ''

  // hidden things across every profile
  // Hidden things, once each: profiles sharing files hold the same ones (复核：计两次)
  const hiddenGroups = $derived(shareGroups().map(g => {
    const conv = new Map<string, { pid: number; c: Conv }>(), dirs = new Map<string, { pid: number; f: HiddenFolder }>()
    let n = 0
    for (const pid of g) {
      const d = hist.byProfile[pid]?.data
      n = Math.max(n, d?.hidden ?? 0)
      for (const c of hist.byProfile[pid]?.hiddenList ?? []) if (!conv.has(c.id)) conv.set(c.id, { pid, c })
      for (const f of d?.hidden_folders ?? []) if (!dirs.has(f.key)) dirs.set(f.key, { pid, f })
    }
    const node = app.nodes.find(x => x.id === app.profiles.find(p => p.id === g[0])?.node_id)?.name ?? ''
    return { node, count: n + dirs.size, convs: [...conv.values()], dirs: [...dirs.values()] }
  }))
  const hiddenCount = $derived(hiddenGroups.reduce((n, g) => n + g.count, 0))
  function toggleHidden() {
    hist.showHidden = !hist.showHidden
    if (hist.showHidden) for (const p of profiles) if ((hist.byProfile[p.id]?.data?.hidden ?? 0) > 0) loadHidden(p.id)
  }
  const anyLoading = $derived(profiles.some(p => hist.byProfile[p.id]?.loading))
</script>

<svelte:window onkeydown={e => { if (e.key === 'Escape' && menu) menu = '' }} />

{#snippet convRow(c: ConvRef, folderLabel: string)}
  <div class="conv" class:sel={selected(c)} class:menuopen={menu === cmenu(c)} data-testid="conv">
    <button class="cmain" onclick={() => openConv(c)} title={convTitle(c)}>
      {#if openConvs.has(c.id)}<span class="live" title="正在一个会话里打开"></span>{/if}
      {#if folderLabel}
        <span class="ctwo"><span class="ctitle">{convTitle(c)}</span><span class="cdir2">📁 {folderLabel}</span></span>
      {:else}
        <span class="ctitle">{convTitle(c)}</span>
      {/if}
      <span class="cago" title={'最后活动：' + fullTime(c.updated)}>{ago(c.updated, hist.tick)}</span>
    </button>
    <button class="dots" title="更多" aria-label="更多" aria-haspopup="menu" aria-expanded={menu === cmenu(c)}
      onclick={e => { e.stopPropagation(); menu = menu === cmenu(c) ? '' : cmenu(c) }} data-testid="conv-menu">⋯</button>
    {#if menu === cmenu(c)}
      <div class="pop" role="menu">
        <button role="menuitem" onclick={e => { e.stopPropagation(); hideConv(c) }} data-testid="menu-hide">隐藏这段对话</button>
      </div>
    {/if}
  </div>
{/snippet}

{#if profiles.length}
  <section class="projects" data-testid="projects">
    <h4>
      项目
      <span class="tools">
        <button class="ic" title="搜索对话" onclick={() => { searching = !searching; if (!searching) hist.query = '' }} data-testid="history-search-toggle">⌕</button>
        <button class="ic" class:spin={anyLoading} title="刷新" onclick={() => loadAll({ force: true })} data-testid="history-refresh">↻</button>
      </span>
    </h4>
    {#if searching}
      <!-- svelte-ignore a11y_autofocus -->
      <input class="search" placeholder="搜索标题、内容或文件夹" bind:value={hist.query} autofocus data-testid="history-search" />
    {/if}

    {#if !list.length}
      <p class="note">{hist.query ? '没有匹配的对话。' : '还没有对话记录。'}</p>
    {/if}

    {#each list as m (m.nodeId)}
      {@const open = openMachines.has(String(m.nodeId)) || !!hist.query}
      <div class="machine" data-testid="machine">
        <button class="mrow" onclick={() => openMachines = flip(openMachines, String(m.nodeId), 'th-history-machines-open')} aria-expanded={open} title={m.online ? '在线' : '离线'}>
          <span class="caret" class:open>›</span>
          <span class="mdot" class:on={m.online}></span>
          <span class="mname">{m.name}</span>
          {#if m.loading}<span class="mspin" title="正在读取"></span>{/if}
          <span class="mago" title={m.updated ? '最近一次：' + fullTime(m.updated) : ''}>{m.updated ? ago(m.updated, hist.tick) : ''}</span>
        </button>

        {#if !open}
          <div class="recent">
            {#each m.recent as c (`${c.profileId}:${c.id}`)}
              {@render convRow(c, m.folders.find(f => f.profileId === c.profileId && f.items.includes(c))?.name ?? '')}
            {/each}
            {#if !m.recent.length && !m.loading}
              <p class="note">{m.errors[0] ?? m.notes[0] ?? (m.online ? '还没有对话。' : '离线，历史读不到。')}</p>
            {/if}
            {#if m.count > m.recent.length}
              <button class="moreb" onclick={() => openMachines = flip(openMachines, String(m.nodeId), 'th-history-machines-open')}>全部 {m.count} 段对话 ›</button>
            {/if}
          </div>
        {:else}
          <div class="mbody">
            {#each m.profiles.filter(p => p.my_folders?.length) as p}
              <div class="scope" title={p.my_folders!.join('\n')} data-testid="history-scope">{m.profiles.length > 1 ? p.name + '：' : ''}只显示 {p.my_folders!.map(f => f.split('\\').pop() || f).join('、')}</div>
            {/each}
            {#each m.errors as err}<p class="note bad">{err} <button class="link" onclick={() => loadAll({ force: true })}>重试</button></p>{/each}
            {#each m.notes as n}<p class="note">{n}</p>{/each}
            {#if m.loading && !m.folders.length}<div class="skeleton"><span></span><span></span></div>{/if}
            {#each m.folders as f (f.key)}
              {@const fopen = !closedFolders.has(f.key) || !!hist.query}
              <div class="group" data-testid="project">
                <div class="prow" class:menuopen={menu === 'f:' + f.key}>
                  <button class="ptitle" onclick={() => closedFolders = flip(closedFolders, f.key, 'th-history-folders-closed')} title={f.cwd} aria-expanded={fopen}>
                    <span class="caret small" class:open={fopen}>›</span>
                    <span class="folder">{fopen ? '📂' : '📁'}</span>
                    <span class="pname">{f.name}</span>
                    {#if m.profiles.length > 1}<span class="pchip">{profileName(f.profileId)}</span>{/if}
                    {#if f.running}<span class="live" title="这个文件夹里有运行中的会话"></span>{/if}
                  </button>
                  <span class="pmeta" title={'最近一次：' + fullTime(f.updated)}>{ago(f.updated, hist.tick)}</span>
                  {#if f.cwd}<!-- a folder of unknown path cannot be named -->
                    <button class="dots" title="更多" aria-label="更多" aria-haspopup="menu" aria-expanded={menu === 'f:' + f.key}
                      onclick={e => { e.stopPropagation(); menu = menu === 'f:' + f.key ? '' : 'f:' + f.key }} data-testid="folder-menu">⋯</button>
                  {/if}
                  {#if menu === 'f:' + f.key}
                    <div class="pop" role="menu">
                      <button role="menuitem" onclick={e => { e.stopPropagation(); hideFolder(f) }} data-testid="menu-hide-folder">隐藏这个文件夹</button>
                      <button role="menuitem" onclick={e => { e.stopPropagation(); renameFolder(f) }} data-testid="menu-rename">重命名（只改显示）</button>
                    </div>
                  {/if}
                </div>
                {#if fopen}
                  <div class="convs">
                    {#if f.cwd && !hist.query}
                      <div class="pacts">
                        <button onclick={() => start(f.profileId, f.cwd, 'continue')} disabled={!m.online || !app.profiles.find(p => p.id === f.profileId)?.continue_cmd}
                          title="在这个文件夹里接着最近的对话" data-testid="project-continue">▶ 继续上次</button>
                        <button onclick={() => start(f.profileId, f.cwd, 'new')} disabled={!m.online}
                          title="在这个文件夹里开一个全新的对话" data-testid="project-new">＋ 新对话</button>
                      </div>
                    {/if}
                    {#each (more[f.key] || hist.query ? f.items : f.items.slice(0, perFolder)) as c (c.id)}
                      {@render convRow(c, '')}
                    {/each}
                    {#if f.items.length > perFolder && !hist.query}
                      <button class="moreb" onclick={() => more[f.key] = !more[f.key]}>{more[f.key] ? '收起' : `展开更多（还有 ${f.items.length - perFolder} 条）`}</button>
                    {/if}
                  </div>
                {/if}
              </div>
            {/each}
          </div>
        {/if}
      </div>
    {/each}

    {#if hiddenCount > 0 || hist.showHidden}
      <button class="hiddenb" onclick={toggleHidden} data-testid="history-hidden-toggle">{hist.showHidden ? '收起已隐藏' : `已隐藏 ${hiddenCount} 项`}</button>
      {#if hist.showHidden}
        <div class="hidden-list">
          {#each hiddenGroups as g}
            {#each g.dirs as { pid, f: hf } (hf.key)}
              <div class="hid">
                <span class="ctitle" title={hf.folder}>📁 {hf.name || hf.folder.split(/[\\/]/).pop()} <span class="cdir">{g.node} · {hf.count} 段</span></span>
                <button class="link" onclick={() => setFolder(pid, hf.folder, { hidden: false })} data-testid="unhide-folder">恢复</button>
              </div>
            {/each}
            {#each g.convs as { pid, c } (c.id)}
              <div class="hid">
                <span class="ctitle" title={c.cwd}>{convTitle(c)} <span class="cdir">{g.node}</span></span>
                <button class="link" onclick={() => setHidden(pid, c, false)} data-testid="unhide">恢复</button>
              </div>
            {/each}
          {/each}
          {#if !hiddenCount}<p class="note">没有隐藏的内容。</p>{/if}
        </div>
      {/if}
    {/if}
  </section>
{/if}

<style>
  .projects { display: flex; flex-direction: column; gap: 4px; min-height: 0; }
  h4 { margin: 8px 0 2px; padding: 0 2px; color: var(--dim); font-weight: 600; font-size: 11px; letter-spacing: .08em; display: flex; align-items: center; gap: 6px; }
  .tools { margin-left: auto; display: flex; gap: 2px; }
  .ic { background: none; border: 1px solid transparent; color: var(--dim); width: 26px; height: 24px; padding: 0; border-radius: 6px; font-size: 13px; line-height: 1; display: inline-flex; align-items: center; justify-content: center; }
  .ic:hover:not(:disabled) { color: var(--fg); background: var(--panel-2); border-color: var(--line); }
  .spin { animation: spin 1s linear infinite; }
  @keyframes spin { to { transform: rotate(360deg); } }
  .search { padding: 6px 9px; font-size: 13px; }
  .note { color: var(--dim); font-size: 12px; margin: 4px 6px; line-height: 1.5; }
  .note.bad { color: var(--bad); }
  .link { background: none; border: none; color: var(--accent); padding: 0 2px; font-size: 12px; flex: none; }
  .skeleton { display: flex; flex-direction: column; gap: 8px; padding: 6px 8px; }
  .skeleton span { height: 12px; border-radius: 6px; background: linear-gradient(90deg, var(--panel-2), var(--line), var(--panel-2)); background-size: 200% 100%; animation: shimmer 1.2s infinite; }
  .skeleton span:nth-child(2) { width: 70%; }
  @keyframes shimmer { to { background-position: -200% 0; } }

  /* machine */
  .machine { border-radius: 10px; }
  .machine + .machine { margin-top: 4px; }
  .mrow { width: 100%; display: flex; align-items: center; gap: 7px; background: var(--bg); border: 1px solid var(--line); border-radius: 9px; padding: 7px 9px; text-align: left; }
  .mrow:hover { border-color: var(--accent); }
  .mdot { width: 8px; height: 8px; border-radius: 50%; background: var(--bad); flex: none; }
  .mdot.on { background: var(--ok); box-shadow: 0 0 0 3px rgba(76, 194, 107, .16); }
  .mname { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 600; font-size: 13px; }
  .mago { font-size: 11px; color: var(--dim); white-space: nowrap; }
  .mspin { width: 10px; height: 10px; border: 2px solid var(--line); border-top-color: var(--accent); border-radius: 50%; animation: spin .8s linear infinite; flex: none; }
  .recent, .mbody { display: flex; flex-direction: column; gap: 1px; padding: 3px 0 2px 10px; }
  .caret { color: var(--dim); width: 10px; display: inline-block; transition: transform .15s; font-size: 14px; flex: none; }
  .caret.open { transform: rotate(90deg); }
  .caret.small { font-size: 12px; }
  .scope { font-size: 11px; color: var(--dim); padding: 2px 6px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

  /* folder */
  .prow { position: relative; display: flex; align-items: center; border-radius: 8px; min-height: 30px; }
  .prow:hover, .prow.menuopen { background: var(--panel-2); }
  .ptitle { flex: 1; min-width: 0; display: flex; align-items: center; gap: 6px; background: none; border: none; padding: 5px 4px; text-align: left; }
  .folder { font-size: 13px; }
  .pname { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 500; }
  .pchip { font-size: 10px; color: var(--dim); border: 1px solid var(--line); border-radius: 999px; padding: 0 6px; white-space: nowrap; flex: none; }
  .pmeta { font-size: 11px; color: var(--dim); white-space: nowrap; padding-right: 2px; }
  .pacts { display: flex; gap: 6px; padding: 3px 0 4px 1px; }
  .pacts button { font-size: 12px; padding: 3px 10px; border-radius: 999px; background: var(--bg); color: var(--dim); }
  .pacts button:hover:not(:disabled) { color: var(--fg); }
  .live { width: 7px; height: 7px; border-radius: 50%; background: var(--ok); flex: none; box-shadow: 0 0 0 3px rgba(76, 194, 107, .18); }
  .convs { display: flex; flex-direction: column; gap: 1px; margin: 1px 0 4px 14px; border-left: 1px solid var(--line); padding-left: 6px; }

  /* conversation */
  .conv { position: relative; display: flex; align-items: center; border: 1px solid transparent; border-radius: 7px; min-height: 30px; }
  .conv:hover, .conv.menuopen { background: var(--panel-2); }
  .conv.sel { background: var(--panel-2); border-color: var(--line); box-shadow: inset 2px 0 0 var(--accent); }
  .cmain { flex: 1; min-width: 0; display: flex; align-items: center; gap: 6px; background: none; border: none; padding: 5px 4px 5px 7px; text-align: left; }
  .cdir { font-size: 11px; color: var(--dim); white-space: nowrap; flex: none; max-width: 40%; overflow: hidden; text-overflow: ellipsis; }
  .ctwo { flex: 1; min-width: 0; display: flex; flex-direction: column; line-height: 1.3; }
  .cdir2 { font-size: 11px; color: var(--dim); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .ctitle { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; }
  .cago { font-size: 11px; color: var(--dim); white-space: nowrap; }

  /* ⋯ and its menu: on hover with a mouse, always on a touch screen */
  .dots { background: none; border: none; color: var(--dim); width: 24px; height: 24px; padding: 0; border-radius: 6px; flex: none; opacity: 0; font-size: 14px; line-height: 1; }
  .conv:hover .dots, .prow:hover .dots, .menuopen .dots, .dots:focus-visible { opacity: 1; }
  .dots:hover { color: var(--fg); background: var(--line); }
  .pop { position: absolute; right: 0; top: calc(100% + 2px); z-index: 12; min-width: 168px; background: var(--panel); border: 1px solid var(--line); border-radius: 9px; padding: 4px; box-shadow: var(--shadow); display: flex; flex-direction: column; }
  .pop button { background: none; border: none; text-align: left; padding: 8px 10px; border-radius: 6px; font-size: 13px; }
  .pop button:hover { background: var(--panel-2); }

  .moreb, .hiddenb { background: none; border: none; color: var(--accent); font-size: 12px; text-align: left; padding: 4px 7px; }
  .hiddenb { color: var(--dim); margin-top: 4px; }
  .hiddenb:hover { color: var(--fg); }
  .hidden-list { display: flex; flex-direction: column; gap: 2px; padding-left: 6px; }
  .hid { display: flex; align-items: center; gap: 6px; padding: 4px 6px; border-radius: 7px; }
  .hid:hover { background: var(--panel-2); }
  @media (hover: none) {
    .dots { opacity: 1; width: 34px; height: 32px; }
    .pacts button { padding: 6px 12px; font-size: 13px; }
    .prow { min-height: 40px; }
    .conv { min-height: 40px; }
    .mrow { padding: 10px; }
    .ic { width: 34px; height: 32px; font-size: 15px; }
  }
</style>
