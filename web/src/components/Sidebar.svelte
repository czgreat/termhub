<script lang="ts" module>
  const handledNew = { n: 0 }
</script>

<script lang="ts">
  import { app, openSession, toast, isActive, plainTitle } from '../lib/state.svelte'
  import StatusDot from '../lib/StatusDot.svelte'
  import UsageBadge from '../lib/UsageBadge.svelte'
  import { post, del } from '../lib/api'
  import Mark from '../lib/Mark.svelte'
  import NewSession from './NewSession.svelte'
  import ProjectList from './ProjectList.svelte'
  import SavedInput from './SavedInput.svelte'
  import { hist } from '../lib/history.svelte'

  let { onadmin, onhelp, openNew = 0 }: { onadmin: () => void; onhelp?: () => void; openNew?: number } = $props()
  let showNew = $state(false)
  // the Ctrl+Shift+U shortcut and ⋮ → 新建会话: each ask opens it once. The
  // sidebar is mounted again with every drawer, so the last ask handled is
  // kept outside it (PWA 复核: the dialog came back with every ☰).
  $effect(() => { if (openNew > handledNew.n) { handledNew.n = openNew; showNew = true } })

  async function logout() {
    await post('/api/auth/logout')
    location.reload()
  }
  async function endSession(sid: string) {
    if (!confirm('结束这个会话？里面运行的程序会被关闭。')) return
    try { await del('/api/sessions/' + sid) } catch (e: any) { toast(e.message) }
  }
  const nodeName = (id: number) => app.nodes.find(n => n.id === id)?.name ?? `节点 ${id}`
  const online = $derived(app.nodes.filter(n => n.online).length)
  // The machines are listed under 项目 with their status, so this list starts
  // closed: one line, red when a node is offline (remembered on this device).
  let showNodes = $state((() => { try { return localStorage.getItem('th-nodes-open') === '1' } catch { return false } })())
  function toggleNodes() {
    showNodes = !showNodes
    try { localStorage.setItem('th-nodes-open', showNodes ? '1' : '0') } catch {}
  }
  const shown = $derived(app.me?.user.display_name || app.me?.user.username || '?')
  const initial = $derived(shown.slice(0, 1).toUpperCase())
</script>

<aside>
  <div class="brand">
    <Mark size={30} />
    <b>termhub</b>
    <span class="ver" title="网页版本">{__TH_VERSION__}</span>
  </div>

  <button class="primary new" onclick={() => showNew = true} data-testid="new-session"><span class="plus">＋</span> 新建会话</button>

  <section>
    <h4><button class="fold" onclick={toggleNodes} aria-expanded={showNodes} data-testid="nodes-toggle">
      <span class="caret" class:open={showNodes}>›</span>节点 <span class="count" class:bad={online < app.nodes.length}>{online}/{app.nodes.length} 在线</span>
    </button></h4>
    {#if showNodes}
    {#each app.nodes as n (n.id)}
      <div class="node" data-testid="node" title={n.online ? '在线' : '离线'}>
        <span class="dot" class:on={n.online}></span>
        <span class="nname">{n.name}</span>
        {#if n.pwsh_store}<span class="warn" title="这台机器的 pwsh 是商店版，无人登录时无法启动；请安装 MSI 版">⚠</span>{/if}
      </div>
    {:else}
      <p class="empty">还没有可用的节点。</p>
    {/each}
    {/if}
  </section>

  <section>
    <h4>会话 {#if app.sessions.length}<span class="count">{app.sessions.length}</span>{/if}</h4>
    {#each app.sessions as s (s.sid)}
      <div class="sess" class:active={isActive(s.sid)} data-testid="session">
        <button class="open" onclick={() => { hist.view = null; openSession(s.sid) }}>
          <div class="stitle"><StatusDot sid={s.sid} />{plainTitle(s.title) || s.profile_name}</div>
          <div class="meta"><span class="chip">{nodeName(s.node_id)}</span><span class="cwd">{s.cwd}</span><UsageBadge sid={s.sid} /></div>
        </button>
        <button class="x" title="结束会话" onclick={() => endSession(s.sid)}>■</button>
      </div>
    {:else}
      <p class="empty">没有运行中的会话。</p>
    {/each}
  </section>

  <SavedInput />
  <div class="grow"><ProjectList /></div>

  <div class="user">
    <span class="avatar">{initial}</span>
    <div class="who">
      <div class="uname" title={app.me?.user.username}>{shown}</div>
      <div class="role">{app.me?.user.role === 'admin' ? '管理员' : '用户'}</div>
    </div>
    <!-- icons with their names as labels: three words beside the name squeezed it to "ab…" and wrapped 管理员 (主人 09-26) -->
    {#if app.me?.user.role === 'admin'}<button class="ghost icon" onclick={onadmin} title="管理：节点、CLI 配置、用户" aria-label="管理">⚙</button>{/if}
    <button class="ghost icon" onclick={() => onhelp?.()} title="使用帮助：常用操作与快捷键" aria-label="帮助" data-testid="sidebar-help">？</button>
    <button class="ghost icon" onclick={logout} title="退出登录" aria-label="退出">⏻</button>
  </div>
</aside>

{#if showNew}<NewSession onclose={() => showNew = false} />{/if}

<style>
  aside { width: 264px; background: var(--panel); border-right: 1px solid var(--line); display: flex; flex-direction: column; padding: 12px; gap: 10px; overflow-y: auto; }
  .brand { display: flex; align-items: center; gap: 9px; padding: 2px 2px 4px; font-size: 16px; }
  .ver { margin-left: auto; font-size: 11px; color: var(--dim); background: var(--bg); border: 1px solid var(--line); border-radius: 999px; padding: 1px 8px; }
  .new { width: 100%; padding: 9px 12px; }
  .plus { font-weight: 700; margin-right: 2px; }
  section { display: flex; flex-direction: column; gap: 2px; }
  .grow { flex: 1; }
  h4 { margin: 8px 0 4px; padding: 0 2px; color: var(--dim); font-weight: 600; font-size: 11px; letter-spacing: .08em; text-transform: uppercase; display: flex; align-items: center; gap: 6px; }
  .fold { display: flex; align-items: center; gap: 6px; background: none; border: none; padding: 0; color: inherit; font: inherit; letter-spacing: inherit; text-transform: inherit; }
  .fold:hover { color: var(--fg); }
  .caret { display: inline-block; transition: transform .15s; font-size: 13px; letter-spacing: 0; }
  .caret.open { transform: rotate(90deg); }
  .count.bad { color: var(--bad); }
  .count { font-weight: 500; letter-spacing: 0; text-transform: none; background: var(--bg); border-radius: 999px; padding: 0 7px; font-size: 11px; }
  .node { display: flex; align-items: center; gap: 8px; padding: 6px 8px; border-radius: 8px; }
  .node:hover { background: var(--panel-2); }
  .nname { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .warn { color: var(--warn); }
  .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--bad); flex: none; box-shadow: 0 0 0 3px rgba(229, 83, 75, .18); }
  .dot.on { background: var(--ok); box-shadow: 0 0 0 3px rgba(76, 194, 107, .18); }
  .empty { color: var(--dim); font-size: 12px; margin: 4px 2px; }
  .sess { display: flex; align-items: stretch; border-radius: 8px; border: 1px solid transparent; transition: background .12s, border-color .12s; }
  .sess:hover { background: var(--panel-2); }
  .sess.active { background: var(--panel-2); border-color: var(--line); box-shadow: inset 3px 0 0 var(--accent); }
  .sess .open { flex: 1; min-width: 0; text-align: left; background: none; border: none; padding: 7px 8px; border-radius: 8px; }
  .stitle { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .meta { display: flex; align-items: center; gap: 6px; margin-top: 3px; font-size: 11px; color: var(--dim); min-width: 0; }
  .chip { flex: none; background: var(--bg); border-radius: 4px; padding: 0 5px; }
  .meta :global(.usage) { margin-left: auto; flex: none; }
  .cwd { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; direction: rtl; text-align: left; }
  .sess .x { background: none; border: none; color: var(--dim); padding: 0 8px; opacity: 0; border-radius: 8px; }
  .sess:hover .x, .sess .x:focus-visible { opacity: 1; }
  .sess .x:hover { color: var(--bad); }
  @media (hover: none) { .sess .x { opacity: 1; } }
  .user { display: flex; align-items: center; gap: 8px; padding: 10px 6px 2px; border-top: 1px solid var(--line); }
  .avatar { width: 30px; height: 30px; border-radius: 50%; background: linear-gradient(135deg, var(--accent), var(--accent-2)); color: #fff; font-weight: 700; display: flex; align-items: center; justify-content: center; flex: none; }
  .who { flex: 1; min-width: 0; }
  .uname { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; }
  .role { font-size: 11px; color: var(--dim); white-space: nowrap; }
  .ghost { background: none; border-color: transparent; color: var(--dim); padding: 5px 8px; }
  .ghost.icon { padding: 4px 7px; font-size: 15px; line-height: 1; flex: none; }
  .ghost:hover { color: var(--fg); border-color: var(--line); background: var(--panel-2); }
</style>
