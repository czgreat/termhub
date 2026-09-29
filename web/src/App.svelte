<script lang="ts">
  // Page frame (docs/M9 第 3 节, M11 第 3 节): on a desktop the sidebar and one
  // or two panes each with its own tabs; on a phone a top bar, a drawer, one
  // pane, the input box and the key bar. Keyboard shortcuts of the app itself
  // all use Ctrl+Shift+letter so they never collide with the terminal (M9 第 5 节).
  import { onMount, untrack } from 'svelte'
  import { app, loadMe, refreshLists, connectEvents, closeTab, sessionOf, activate, focusPane, setSplit, cycleTab, mountedSet, restoreLayout, saveLayout, otherPane, openSession, initDevice, watchVisibility, setInputMode, focusedSid, toast, statusOf, plainTitle, watchUsage, type Status } from './lib/state.svelte'
  import StatusDot from './lib/StatusDot.svelte'
  import UsageBadge from './lib/UsageBadge.svelte'
  import { usageTip } from './lib/usage'
  import { terms } from './lib/terms'
  import { del } from './lib/api'
  import { reverify, submitReverify, cancelReverify } from './lib/reverify.svelte'
  import Login from './components/Login.svelte'
  import Sidebar from './components/Sidebar.svelte'
  import Terminal from './components/Terminal.svelte'
  import Admin from './components/Admin.svelte'
  import KeyBar from './components/KeyBar.svelte'
  import Composer from './components/Composer.svelte'
  import OptionBar from './components/OptionBar.svelte'
  import Guide from './components/Guide.svelte'
  import HistoryView from './components/HistoryView.svelte'
  import { hist } from './lib/history.svelte'

  let ready = $state(false)
  let showAdmin = $state(false)
  let newSession = $state(0) // bumps to ask the sidebar to open "new session"
  let code = $state('')
  let restored = 0 // the user whose layout was restored
  let titleMenu = $state(false), moreMenu = $state(false)
  let showGuide = $state(false)

  // Android's back button and the iPhone's back swipe close what is open on
  // top (the drawer, a record, a menu) instead of leaving the app: each open
  // layer holds one history entry (PWA 复核).
  const layers = $derived(!app.mobile ? 0 : (app.drawer ? 1 : 0) + (hist.view ? 1 : 0) + (titleMenu || moreMenu ? 1 : 0))
  let pushed = 0, skip = 0
  $effect(() => {
    const d = layers
    untrack(() => {
      while (pushed < d) { history.pushState({ th: true }, ''); pushed++ }
      while (pushed > d) { skip++; pushed--; history.back() } // closed by hand: take its entry back
    })
  })
  onMount(() => {
    const pop = () => {
      if (skip) { skip--; return }
      pushed = Math.max(0, pushed - 1)
      if (titleMenu || moreMenu) { titleMenu = moreMenu = false }
      else if (hist.view) hist.view = null
      else if (app.drawer) app.drawer = false
    }
    window.addEventListener('popstate', pop)
    // a tap outside the top bar's menus closes them
    const outside = (e: Event) => {
      if (!(titleMenu || moreMenu)) return
      const t = e.target as Element | null
      if (t?.closest('.menu, [data-testid=title], [data-testid=more]')) return
      titleMenu = moreMenu = false
    }
    document.addEventListener('pointerdown', outside, true)
    return () => { window.removeEventListener('popstate', pop); document.removeEventListener('pointerdown', outside, true) }
  })

  // The guide opens by itself once per user and device; closing it (either
  // button) counts as seen. Storage may be unavailable: then it just opens.
  const guideKey = () => `th-guide-seen:${app.me?.user.id ?? ''}`
  let guideChecked = false
  $effect(() => {
    if (guideChecked || !app.me || app.me.pending.length) return
    guideChecked = true
    let seen = false
    try { seen = localStorage.getItem(guideKey()) === '1' } catch {}
    if (!seen) showGuide = true
  })
  function closeGuide() {
    showGuide = false
    try { localStorage.setItem(guideKey(), '1') } catch {}
  }

  onMount(() => {
    initDevice()
    watchVisibility()
    watchUsage()
    loadMe().then(() => ready = true)
    // Offline shell: keep trying to reach the Hub (docs/M11 第 2 节).
    const retry = () => { if (app.offline) loadMe() }
    window.addEventListener('online', retry)
    const t = setInterval(retry, 5000)
    // Soft keyboard: the layout follows the visual viewport (docs/M11 第 4 节).
    const vv = window.visualViewport
    const fitVV = () => {
      if (!app.mobile || !vv) { document.documentElement.style.removeProperty('--vvh'); return }
      document.documentElement.style.setProperty('--vvh', vv.height + 'px')
      window.scrollTo(0, 0)
    }
    vv?.addEventListener('resize', fitVV)
    vv?.addEventListener('scroll', fitVV)
    fitVV()
    return () => { clearInterval(t); window.removeEventListener('online', retry); vv?.removeEventListener('resize', fitVV); vv?.removeEventListener('scroll', fitVV) }
  })

  $effect(() => {
    if (app.me && app.me.pending.length === 0) {
      refreshLists()
      connectEvents()
    }
  })

  // A superseded refresh may finish before the current snapshot is applied.
  // Restore only when that snapshot is ready, otherwise all saved tabs look dead.
  $effect(() => {
    const id = app.me?.user.id
    if (id && app.listsUser === id && restored !== id) {
      restored = id
      untrack(restoreLayout)
    }
  })

  const mounted = $derived(mountedSet())
  const active = $derived(app.panes[0]?.active ?? '')

  function title(sid: string) {
    const s = sessionOf(sid)
    if (!s) return sid.slice(0, 8)
    const name = plainTitle(s.title) ||`${s.profile_name} · ${s.cwd.split('\\').pop() || s.cwd}`
    return app.ended[sid] ? `${name}（已结束）` : name
  }

  // Sessions waiting for the owner, in the browser tab's title and on the
  // phone's top bar (状态点): ask before done.
  const waiting = $derived(app.sessions.map(s => statusOf(s.sid)).filter(st => st === 'ask' || st === 'done'))
  const othersWaiting = $derived<Status>(app.sessions.some(s => s.sid !== active && statusOf(s.sid) === 'ask') ? 'ask'
    : app.sessions.some(s => s.sid !== active && statusOf(s.sid) === 'done') ? 'done' : '')
  $effect(() => { document.title = waiting.length ? `(${waiting.length}) termhub` : 'termhub' })

  /** Pastes the focused terminal's selection into the other pane's terminal (docs/M9 第 6 节). */
  function sendToOther() {
    const from = terms.get(app.panes[app.focus]?.active ?? '')
    const to = terms.get(app.panes[otherPane()]?.active ?? '')
    const text = from?.selection() ?? ''
    if (!to || !text) return
    to.paste(text)
    to.focus()
  }

  // Ctrl+Shift+letter shortcuts. Letters the browser reserves for itself
  // (N, T, W, Q) are avoided; C is copy inside the terminal.
  function onKey(e: KeyboardEvent) {
    if (!e.ctrlKey || !e.shiftKey || e.altKey || e.metaKey) return
    const k = e.key.toUpperCase()
    let done = true
    switch (k) {
      case 'U': newSession++; break
      case 'D': if (!app.mobile) setSplit(app.split === 'none' ? 'row' : 'none'); break
      case 'O': if (app.panes.length > 1) focusPane(1 - app.focus); terms.get(app.panes[app.focus]?.active ?? '')?.focus(); break
      case 'H': cycleTab(-1); break
      case 'L': cycleTab(1); break
      case 'S': sendToOther(); break
      default: done = false
    }
    if (done) { e.preventDefault(); e.stopPropagation() }
  }

  // Divider drag: the ratio is the first pane's share.
  let panesEl: HTMLDivElement
  function startDrag(e: PointerEvent) {
    e.preventDefault()
    const rect = panesEl.getBoundingClientRect()
    const move = (ev: PointerEvent) => {
      const v = app.split === 'row' ? (ev.clientX - rect.left) / rect.width : (ev.clientY - rect.top) / rect.height
      app.ratio = Math.min(0.85, Math.max(0.15, v))
    }
    const up = () => { window.removeEventListener('pointermove', move); window.removeEventListener('pointerup', up); saveLayout() }
    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', up)
  }

  // Phone: a swipe from the left edge opens the drawer (docs/M11 第 7 节).
  let edgeX = -1
  function touchStart(e: TouchEvent) { edgeX = e.touches[0].clientX < 24 ? e.touches[0].clientX : -1 }
  function touchEnd(e: TouchEvent) {
    if (edgeX >= 0 && e.changedTouches[0].clientX - edgeX > 60) app.drawer = true
    edgeX = -1
  }

  async function endActive() {
    if (!active || app.ended[active]) return
    if (!confirm('结束这个会话？里面运行的程序会被关闭。')) return
    try { await del('/api/sessions/' + active) } catch (e: any) { toast(e.message) }
  }
  async function logout() {
    await import('./lib/api').then(m => m.post('/api/auth/logout'))
    location.reload()
  }
</script>

<svelte:window onkeydown={onKey} ontouchstart={touchStart} ontouchend={touchEnd} />

{#if !ready}
  <p class="muted" style="padding:2em">加载中…</p>
{:else if app.offline && !app.me}
  <div class="offline" data-testid="offline">
    <h3>无法连接</h3>
    <p class="muted">连不上 termhub 服务。网络恢复后会自动重试。</p>
    <button onclick={() => loadMe()}>立即重试</button>
  </div>
{:else if !app.me || app.me.pending.length}
  <Login />
{:else if app.mobile}
  <div class="m-layout">
    <header class="topbar">
      <button class="icon" data-testid="drawer-open" title="节点与会话" onclick={() => app.drawer = true}>☰</button>
      <button class="title" data-testid="title" onclick={() => { titleMenu = !titleMenu; moreMenu = false }}>
        {#if active}<StatusDot sid={active} />{/if}{active ? title(active) : 'termhub'} {#if active}<UsageBadge sid={active} />{/if} <span class="muted">▾</span>{#if othersWaiting}<StatusDot st={othersWaiting} />{/if}
      </button>
      <span class="dot {app.linkState[active] ?? ''}" title={app.linkState[active] ?? ''}></span>
      <button class="icon" data-testid="input-mode" title={app.inputMode === 'box' ? '当前：输入框方式，点击改为直接输入' : '当前：直接输入，点击改为输入框方式'}
        onclick={() => setInputMode(app.inputMode === 'box' ? 'direct' : 'box')}>{app.inputMode === 'box' ? '▤' : '⌨'}</button>
      <button class="icon" data-testid="more" title="更多" onclick={() => { moreMenu = !moreMenu; titleMenu = false }}>⋮</button>
    </header>
    {#if titleMenu}
      <div class="menu" data-testid="title-menu">
        {#each app.panes[0].tabs as sid (sid)}
          <button class:active={sid === active} onclick={() => { hist.view = null; openSession(sid); titleMenu = false }}><StatusDot {sid} />{title(sid)} <UsageBadge {sid} /></button>
        {:else}
          <span class="muted">没有打开的会话</span>
        {/each}
      </div>
    {/if}
    {#if moreMenu}
      <div class="menu right" data-testid="more-menu">
        <button onclick={() => { moreMenu = false; app.drawer = true; newSession++ }}>新建会话</button>
        {#if active}<button onclick={() => { moreMenu = false; endActive() }} data-testid="end-active">结束会话</button>
        <button onclick={() => { moreMenu = false; closeTab(active) }}>关闭标签</button>{/if}
        {#if app.me?.user.role === 'admin'}<button onclick={() => { moreMenu = false; showAdmin = true }}>管理</button>{/if}
        <button onclick={() => { moreMenu = false; showGuide = true }} data-testid="help">使用帮助</button>
        <button onclick={logout}>退出登录</button>
      </div>
    {/if}
    <div class="m-terms" data-testid="pane">
      {#each app.panes[0].tabs as sid (sid)}
        <div class="slot" style:display={sid === active ? 'block' : 'none'}>
          <Terminal {sid} pane={0} visible={sid === active} attached={mounted.has(sid)} />
        </div>
      {:else}
        <p class="muted" style="padding:2em;text-align:center">点左上角 ☰ 新建或打开一个会话</p>
      {/each}
      <!-- the cards lie over the terminal's bottom rows rather than pushing
           them up: a resize would make the CLI redraw, the menu would vanish
           for an instant and the cards would jump in and out -->
      {#if active}<OptionBar sid={active} />{/if}
    </div>
    {#if active && app.inputMode === 'box'}<Composer sid={active} />{/if}
    <KeyBar />
    {#if hist.view}<HistoryView />{/if}
    {#if app.drawer}
      <div class="drawer-bg" onclick={() => app.drawer = false} role="presentation">
        <div class="drawer" onclick={e => e.stopPropagation()} role="presentation">
          <Sidebar onadmin={() => { app.drawer = false; showAdmin = true }} onhelp={() => { app.drawer = false; showGuide = true }} openNew={newSession} />
        </div>
      </div>
    {/if}
  </div>
{:else}
  <div class="layout">
    <Sidebar onadmin={() => showAdmin = true} onhelp={() => showGuide = true} openNew={newSession} />
    <main>
      <div class="panes split-{app.split}" bind:this={panesEl}>
        {#each app.panes as pane, pi (pi)}
          {#if pi === 1}
            <div class="divider" onpointerdown={startDrag} role="separator" aria-orientation={app.split === 'row' ? 'vertical' : 'horizontal'}></div>
          {/if}
          <section class="pane" class:focused={app.panes.length > 1 && pi === app.focus} data-testid="pane"
            style={app.split === 'row' ? `width:${(pi === 0 ? app.ratio : 1 - app.ratio) * 100}%` : app.split === 'col' ? `height:${(pi === 0 ? app.ratio : 1 - app.ratio) * 100}%` : ''}
            onpointerdown={() => focusPane(pi)}>
            <div class="tabs" role="tablist">
              {#each pane.tabs as sid (sid)}
                <div class="tab" class:active={sid === pane.active} role="tab" aria-selected={sid === pane.active}>
                  <button class="tabname" title={usageTip(app.usage[sid])} onclick={() => { hist.view = null; activate(sid, pi) }}>
                    <StatusDot {sid} />{#if app.marks[sid] === 'bell' || (!statusOf(sid) && app.marks[sid])}<span class="mark" title={app.marks[sid] === 'bell' ? '响铃' : '有新输出'}>{app.marks[sid] === 'bell' ? '🔔' : '●'}</span>{/if}{title(sid)}
                  </button>
                  <button class="close" title="关闭标签（会话继续运行）" onclick={() => closeTab(sid)}>×</button>
                </div>
              {/each}
              {#if pane.tabs.length === 0}
                <span class="muted" style="padding:6px 10px">在左侧新建或打开一个会话</span>
              {/if}
              <!-- the active tab's cost and context share, at the right of its pane (主人 09-26) -->
              <span class="pane-usage">{#if pane.active}<UsageBadge sid={pane.active} />{/if}</span>
              {#if pi === 0}
                <span class="tools">
                  {#if app.split === 'none'}
                    <button title="左右分屏（Ctrl+Shift+D）" data-testid="split-row" onclick={() => setSplit('row')}>◫</button>
                    <button title="上下分屏" data-testid="split-col" onclick={() => setSplit('col')}>⊟</button>
                  {:else}
                    <button title={app.split === 'row' ? '改为上下分屏' : '改为左右分屏'} onclick={() => setSplit(app.split === 'row' ? 'col' : 'row')}>{app.split === 'row' ? '⊟' : '◫'}</button>
                    <button title="取消分屏（标签合并到一个窗格）" data-testid="unsplit" onclick={() => setSplit('none')}>▣</button>
                  {/if}
                  <button title="使用帮助：常用操作与快捷键" data-testid="help" onclick={() => showGuide = true}>？</button>
                </span>
              {/if}
            </div>
            <div class="terms">
              {#each pane.tabs as sid (sid)}
                <div class="slot" style:display={sid === pane.active ? 'block' : 'none'}>
                  <Terminal {sid} pane={pi} visible={sid === pane.active} attached={mounted.has(sid)} onsend={sendToOther} />
                </div>
              {/each}
            </div>
          </section>
        {/each}
      </div>
      {#if app.touch}<KeyBar />{/if}
      {#if hist.view}<HistoryView />{/if}
    </main>
  </div>
{/if}
{#if showAdmin}
  <Admin onclose={() => showAdmin = false} />
{/if}
{#if showGuide}
  <Guide onclose={closeGuide} />
{/if}

{#if reverify.open}
  <div class="modal-bg">
    <form class="card modal" onsubmit={e => { e.preventDefault(); submitReverify(code); code = '' }}>
      <h3>再次验证</h3>
      <p class="muted">这个操作需要输入验证器里的动态码（或一个恢复码）。</p>
      <input placeholder="6 位动态码" autocomplete="one-time-code" bind:value={code} />
      {#if reverify.error}<p class="err">{reverify.error}</p>{/if}
      <div class="row" style="justify-content:flex-end;margin-top:12px">
        <button type="button" onclick={cancelReverify}>取消</button>
        <button class="primary" type="submit">确认</button>
      </div>
    </form>
  </div>
{/if}
{#if app.updateReady}
  <div class="toast update" data-testid="update">有新版本 <button onclick={() => app.applyUpdate?.()}>点击刷新</button></div>
{/if}
{#if app.toast}<div class="toast" data-testid="toast">{app.toast}</div>{/if}

<style>
  .layout { display: flex; height: 100%; }
  main { flex: 1; display: flex; flex-direction: column; min-width: 0; position: relative; }
  .panes { flex: 1; display: flex; min-height: 0; }
  .panes.split-col { flex-direction: column; }
  .pane { display: flex; flex-direction: column; min-width: 0; min-height: 0; flex: 1; }
  .panes.split-row .pane, .panes.split-col .pane { flex: none; }
  .pane.focused .tabs { box-shadow: inset 0 2px 0 var(--accent); }
  .divider { background: var(--line); flex: none; }
  .panes.split-row .divider { width: 4px; cursor: col-resize; }
  .panes.split-col .divider { height: 4px; cursor: row-resize; }
  .divider:hover { background: var(--accent); }
  .tabs { display: flex; gap: 2px; background: var(--panel); border-bottom: 1px solid var(--line); padding: 4px 6px 0; overflow-x: auto; flex: none; }
  .tab { display: flex; align-items: center; border: 1px solid transparent; border-bottom: none; border-radius: 6px 6px 0 0; white-space: nowrap; }
  .tab.active { background: var(--bg); border-color: var(--line); }
  .tab button { background: none; border: none; padding: 6px 8px; color: var(--dim); }
  .tab.active button { color: var(--fg); }
  .tab .close { padding: 6px; }
  .mark { color: var(--accent); margin-right: 4px; font-size: 11px; }
  .pane-usage { margin-left: auto; align-self: center; padding: 0 6px; }
  .tools { display: flex; gap: 2px; }
  .tools button { background: none; border: none; color: var(--dim); padding: 4px 6px; }
  .tools button:hover { color: var(--fg); }
  .terms { flex: 1; position: relative; min-height: 0; }
  .slot { position: absolute; inset: 0; }
  .offline { padding: 3em 2em; max-width: 420px; }
  .update { bottom: auto; top: calc(12px + env(safe-area-inset-top)); }
  .update button { margin-left: 8px; }

  /* phone */
  .m-layout { display: flex; flex-direction: column; height: var(--vvh, 100%); padding-left: env(safe-area-inset-left); padding-right: env(safe-area-inset-right); }
  .topbar { display: flex; align-items: center; gap: 4px; padding: calc(4px + env(safe-area-inset-top)) 6px 4px; background: var(--panel); border-bottom: 1px solid var(--line); flex: none; }
  .topbar .icon { width: 38px; height: 34px; padding: 0; font-size: 18px; background: none; border: none; }
  .topbar .title { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; text-align: left; background: none; border: none; padding: 6px 4px; }
  .topbar .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--dim); flex: none; }
  .topbar .dot.open { background: var(--ok); }
  .topbar .dot.connecting, .topbar .dot.closed { background: var(--warn); }
  .menu { position: absolute; top: calc(42px + env(safe-area-inset-top)); left: 6px; right: 6px; z-index: 15; background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 6px; display: flex; flex-direction: column; gap: 2px; box-shadow: 0 6px 24px rgba(0,0,0,.4); }
  .menu.right { left: auto; width: 200px; }
  .menu button { text-align: left; background: none; border: none; padding: 10px; }
  .menu button.active { color: var(--accent); }
  .m-terms { flex: 1; position: relative; min-height: 0; }
  .drawer-bg { position: fixed; inset: 0; background: rgba(0,0,0,.5); z-index: 18; }
  .drawer { position: absolute; top: 0; bottom: 0; left: 0; width: min(300px, 85vw); background: var(--panel); box-shadow: 4px 0 24px rgba(0,0,0,.5); display: flex; padding-top: env(safe-area-inset-top); }
  .drawer :global(aside) { width: 100%; border-right: none; }
</style>
