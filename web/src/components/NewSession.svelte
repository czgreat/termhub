<script lang="ts">
  // New session (docs/M10 第 2、3 节): a profile, a folder, and how to start:
  // a new conversation, the folder's last one, or the CLI's own picker.
  import { untrack } from 'svelte'
  import { get, post } from '../lib/api'
  import { app, openSession, toast } from '../lib/state.svelte'
  import { hist, load, folderKey, ago, busyElsewhere, fresh, folderLast, openInFolder, folderTaken } from '../lib/history.svelte'
  import DirPicker from './DirPicker.svelte'

  let { onclose }: { onclose: () => void } = $props()
  let profileId = $state(app.profiles[0]?.id ?? 0)
  let cwd = $state('')
  type Mode = 'new' | 'continue' | 'pick'
  let mode = $state<Mode>('new'), chosen = $state(false) // chosen: the user picked a mode by hand
  let recent = $state<string[]>([]), desktop = $state('') // desktop: the node user's, "" when unknown
  let error = $state(''), busy = $state(false), browsing = $state(false)

  const profile = $derived(app.profiles.find(p => p.id === profileId))
  // A user kept to some folders (docs/M10 第 3.4 节) is offered the desktop
  // only inside them: a conversation opened above them would not show in
  // their 项目, and the desktop may be shared with others (桌面按钮复核).
  const desktopOk = $derived(!!desktop && (!profile?.my_folders?.length || profile.my_folders.some(f => {
    const k = folderKey(f), d = folderKey(desktop)
    return d === k || d.startsWith(k + '\\')
  })))
  const node = $derived(app.nodes.find(n => n.id === profile?.node_id))

  // Only a different choice of profile resets the folder. The profile list is
  // re-fetched whenever a session or node changes (a new object each time),
  // and that must not throw away a folder the user has just picked.
  $effect(() => {
    const id = profileId
    untrack(() => {
      const p = app.profiles.find(p => p.id === id)
      if (!p) return
      cwd = p.my_folders?.[0] ?? p.default_cwd // this user's range first (docs/M10 第 3.4 节)
      get('/api/recent-dirs?node_id=' + p.node_id).then(r => recent = r.dirs ?? [])
      // 桌面 next to the recent folders (主人 2026-09-25); an offline or older
      // node has none, and a slow answer for a profile no longer chosen is dropped.
      desktop = ''
      get(`/api/nodes/${p.node_id}/fs/drives`).then(r => { if (profileId === id) desktop = r.desktop ?? '' }, () => {})
    })
  })

  // What the folder's history says: whether there is a last conversation to
  // continue (a CLI without history is taken at its word), and whether a
  // session of this profile already runs there (it may be that conversation).
  const hasHistory = $derived(profile?.kind === 'claude' || profile?.kind === 'codex')
  $effect(() => { const p = profile; if (p && hasHistory) untrack(() => { if (!hist.byProfile[p.id]?.at) load(p.id, { quiet: true }) }) })
  // what "continue" would resume, a hidden conversation included (复核)
  const resumes = $derived(profile && cwd ? folderLast(profile.id, cwd) : undefined)
  const historyKnown = $derived(!hasHistory || !!(profile && hist.byProfile[profile.id]?.data))
  const canContinue = $derived(!!profile?.continue_cmd && (!hasHistory || !historyKnown || !!resumes))
  // a session of this user in the folder: for Claude Code and Codex the only
  // one it may have there (same kind, any profile; starting goes to it), for
  // another CLI one of this profile, a warning only
  const runningHere = $derived(!profile || !cwd ? '' : hasHistory ? openInFolder(profile.id, cwd)
    : app.sessions.find(s => s.profile_id === profileId && folderKey(s.cwd) === folderKey(cwd))?.sid ?? '')
  $effect(() => {
    const want: Mode = canContinue && (!hasHistory || resumes) ? 'continue' : 'new'
    const pickable = !!profile?.resume_cmd
    // a choice the new profile or folder cannot honour goes back to the default
    untrack(() => { if (!chosen || (mode === 'continue' && !canContinue) || (mode === 'pick' && !pickable)) mode = want })
  })
  function choose(m: Mode) { mode = m; chosen = true }

  async function start() {
    if (busy) return
    error = ''; busy = true // taken at once: a second press while the lists are read would start twice
    try {
      // one of this kind already runs in the folder: go there (the Hub would refuse a second)
      const sid = profile && hasHistory ? openInFolder(profile.id, cwd) : ''
      if (sid) { toast(folderTaken); openSession(sid); onclose(); return }
      if (mode === 'continue' && profile && hasHistory) await fresh(profile.id)
      if (mode === 'continue' && !runningHere && resumes && profile && busyElsewhere(profile.id, resumes)
        && !confirm('这个文件夹最近的对话可能正在别处进行。两处同时继续，两边都会出问题。\n\n仍要在这里继续吗？')) return
      const r = await post('/api/sessions', { profile_id: profileId, cwd, cols: 120, rows: 32, mode })
      openSession(r.session.sid)
      onclose()
    } catch (e: any) { error = e.message } finally { busy = false }
  }
</script>

<div class="modal-bg">
  <div class="card modal">
    <h3>新建会话</h3>
    {#if app.profiles.length === 0}
      <p class="muted">没有绑定给你的 CLI 配置。请让管理员在"管理"里创建并绑定。</p>
    {:else}
      <label for="prof">CLI 配置</label>
      <select id="prof" bind:value={profileId}>
        {#each app.profiles as p (p.id)}
          <option value={p.id}>{app.nodes.find(n => n.id === p.node_id)?.name ?? p.node_id} · {p.name}</option>
        {/each}
      </select>
      {#if node && !node.online}<p class="err">这个节点当前离线。</p>{/if}

      <label for="cwd">工作目录</label>
      <div class="row">
        <input id="cwd" bind:value={cwd} placeholder="C:\Users\...\项目" />
        <button onclick={() => browsing = true} disabled={!node?.online} style="white-space:nowrap;flex:none">浏览…</button>
      </div>
      {#if recent.length || desktopOk}
        <div class="recent">
          {#if desktopOk}<button class="chip" onclick={() => cwd = desktop} title={desktop} data-testid="cwd-desktop">🖥 桌面</button>{/if}
          {#each recent as d}<button class="chip" onclick={() => cwd = d} title={d}>{d.split('\\').pop() || d}</button>{/each}
        </div>
      {/if}

      {#if profile?.resume_cmd || profile?.continue_cmd}
        <div class="lbl">怎么开始</div>
        <div class="seg" role="radiogroup" aria-label="怎么开始">
          <button role="radio" aria-checked={mode === 'new'} class:on={mode === 'new'} onclick={() => choose('new')} data-testid="mode-new">新对话</button>
          <button role="radio" aria-checked={mode === 'continue'} class:on={mode === 'continue'} onclick={() => choose('continue')} disabled={!canContinue}
            title={canContinue ? '' : '这个文件夹里还没有对话'} data-testid="mode-continue">继续上次</button>
          {#if profile?.resume_cmd}
            <button role="radio" aria-checked={mode === 'pick'} class:on={mode === 'pick'} onclick={() => choose('pick')} data-testid="mode-pick">官方选择界面</button>
          {/if}
        </div>
        <p class="hint">
          {#if mode === 'continue'}
            {#if resumes && !resumes.hidden}接着这个文件夹最近的对话：<b>{resumes.title || resumes.first || '（无标题）'}</b>（{ago(resumes.updated, hist.tick)}{ago(resumes.updated, hist.tick) === '刚刚' ? '' : '前'}）{:else if resumes}接着这个文件夹最近的一次对话（{ago(resumes.updated, hist.tick)}{ago(resumes.updated, hist.tick) === '刚刚' ? '' : '前'}，它在列表里已隐藏）。{:else}接着这个文件夹最近的一次对话。{/if}
          {:else if mode === 'pick'}在终端里用 CLI 自带的界面选一个历史对话。想先看内容，可以在左侧“项目”里点开。
          {:else}在这个文件夹里开一个全新的对话。{#if hasHistory && historyKnown && !resumes && cwd} 这个文件夹还没有对话记录。{/if}
          {/if}
        </p>
        {#if mode === 'continue' && !runningHere && resumes && profile && busyElsewhere(profile.id, resumes, hist.tick)}
          <p class="warn" data-testid="busy-elsewhere">这段对话 {ago(resumes.updated, hist.tick)}{ago(resumes.updated, hist.tick) === '刚刚' ? '' : '前'}还有新内容，可能正在别处进行（比如那台电脑自己的终端）。两处同时继续，两边都会出问题；请先在那边退出。</p>
        {/if}
        {#if runningHere && hasHistory}
          <p class="warn" data-testid="folder-taken">这个文件夹里已经有一个运行中的会话，同一个文件夹同时只能开一个，点“启动”会切换过去。
            <button class="link" onclick={() => { openSession(runningHere); onclose() }}>切换过去</button></p>
        {:else if runningHere && mode !== 'new'}
          <p class="warn">这个文件夹里已经有一个运行中的会话，继续上次可能接上的正是它，两个窗口会写同一段对话。
            <button class="link" onclick={() => { openSession(runningHere); onclose() }}>切换过去</button></p>
        {/if}
      {/if}

      {#if error}<p class="err">{error}</p>{/if}
      <div class="row" style="justify-content:flex-end;margin-top:14px">
        <button onclick={onclose}>取消</button>
        <button class="primary" onclick={start} disabled={busy || !cwd || !node?.online} data-testid="start-session">启动</button>
      </div>
    {/if}
  </div>
</div>

{#if browsing && node}
  <DirPicker nodeId={node.id} start={cwd} onpick={p => { cwd = p; browsing = false }} onclose={() => browsing = false} />
{/if}

<style>
  .recent { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 6px; }
  .lbl { margin: 14px 0 6px; color: var(--dim); font-size: 12px; }
  .seg { display: flex; background: var(--bg); border: 1px solid var(--line); border-radius: 10px; padding: 3px; gap: 3px; }
  .seg button { flex: 1; background: none; border: none; border-radius: 7px; padding: 7px 6px; color: var(--dim); white-space: nowrap; }
  .seg button.on { background: var(--panel-2); color: var(--fg); box-shadow: 0 1px 0 rgba(255,255,255,.04), inset 0 0 0 1px var(--line); font-weight: 600; }
  .seg button:hover:not(.on):not(:disabled) { color: var(--fg); }
  .hint { font-size: 12px; color: var(--dim); margin: 7px 2px 0; line-height: 1.5; overflow-wrap: anywhere; }
  .hint b { color: var(--fg); font-weight: 500; }
  .warn { font-size: 12px; color: var(--warn); margin: 6px 2px 0; line-height: 1.5; }
  .link { background: none; border: none; color: var(--accent); padding: 0 2px; font-size: 12px; }
  .chip { font-size: 12px; padding: 2px 8px; max-width: 200px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
