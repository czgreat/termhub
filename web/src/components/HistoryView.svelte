<script lang="ts">
  // One past conversation, read-only (docs/M10 第 3.2 节): the latest messages
  // first, earlier ones on request; "继续这个对话" reopens it in its folder
  // with the CLI's own resume command. Nothing here starts a CLI by itself.
  import { tick } from 'svelte'
  import { app } from '../lib/state.svelte'
  import { hist, readTranscript, setHidden, convTitle, clean, ago, fullTime, start, openSessionFor, busyElsewhere, type Message } from '../lib/history.svelte'

  const v = $derived(hist.view!)
  const profile = $derived(app.profiles.find(p => p.id === v.profileId))
  const node = $derived(app.nodes.find(n => n.id === profile?.node_id))
  const openSid = $derived(openSessionFor(v.profileId, v.conv.id))
  const isHidden = $derived(!!hist.byProfile[v.profileId]?.hiddenList?.some(c => c.id === v.conv.id)
    && !hist.byProfile[v.profileId]?.data?.items.some(c => c.id === v.conv.id))

  let messages = $state<Message[]>([])
  let cursor = 0, more = $state(false)
  let loading = $state(true), older = $state(false), error = $state(''), busy = $state(false)
  let body: HTMLDivElement

  // Each conversation shown gets a generation; an answer for an earlier one
  // (a slow "加载更早" after switching) is dropped, not spliced in (复核).
  let gen = 0
  $effect(() => {
    const { profileId, conv } = v
    gen++
    messages = []; more = false; error = ''; loading = true; older = false
    readTranscript(profileId, conv.id).then(async t => {
      if (hist.view?.conv.id !== conv.id) return
      messages = t.messages; cursor = t.cursor; more = t.more
      loading = false
      await tick(); if (body) body.scrollTop = body.scrollHeight
    }).catch(e => { if (hist.view?.conv.id === conv.id) { error = e.message; loading = false } })
  })

  async function loadOlder() {
    if (older || !more) return
    older = true
    const g = gen
    try {
      const t = await readTranscript(v.profileId, v.conv.id, cursor)
      if (g !== gen) return
      const h = body.scrollHeight
      messages = [...t.messages, ...messages]; cursor = t.cursor; more = t.more
      await tick(); body.scrollTop += body.scrollHeight - h // keep what was on screen in place
    } catch (e: any) { if (g === gen) error = e.message } finally { if (g === gen) older = false }
  }

  async function resume() {
    busy = true
    try { await start(v.profileId, v.conv.cwd, 'resume', v.conv.id) } finally { busy = false }
  }
  async function hide() {
    const hiding = !isHidden, conv = v.conv // read before: once hidden, isHidden turns true
    // closes this record only, not one the user opened meanwhile
    if (await setHidden(v.profileId, conv, hiding) && hiding && hist.view?.conv.id === conv.id) hist.view = null
  }
  const close = () => hist.view = null

  // Text with ``` fences: code blocks apart, the rest as written. Never HTML.
  type Part = { code: boolean; lang?: string; text: string }
  function parts(raw: string): Part[] {
    const text = clean(raw)
    const out: Part[] = []
    const re = /```([\w+-]*)\n?([\s\S]*?)(```|$)/g
    let last = 0
    for (let m; (m = re.exec(text));) {
      if (m.index > last) out.push({ code: false, text: text.slice(last, m.index) })
      out.push({ code: true, lang: m[1], text: m[2].replace(/\n$/, '') })
      last = re.lastIndex
      if (m[0].length === 0) break
    }
    if (last < text.length) out.push({ code: false, text: text.slice(last) })
    return out.map(p => p.code ? p : { ...p, text: p.text.replace(/^\n+|\n+$/g, '') }).filter(p => p.code || p.text)
  }
  const clock = (t?: number) => t ? new Date(t * 1000).toLocaleString('zh-CN', { hour12: false, month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : ''
</script>

<div class="hv" class:mobile={app.mobile} data-testid="history-view">
  <header>
    <button class="back" onclick={close} title="返回终端" aria-label="返回">{app.mobile ? '‹' : '×'}</button>
    <div class="head">
      <h2 title={convTitle(v.conv)}>{convTitle(v.conv)}</h2>
      <div class="meta">
        <span class="chip" title={v.conv.cwd}>📁 {v.conv.cwd || '目录未知'}</span>
        <span>{node?.name ?? ''} · {profile?.name ?? ''}</span>
        <span title={fullTime(v.conv.updated)}>最后活动 {ago(v.conv.updated, hist.tick)}{ago(v.conv.updated, hist.tick) === '刚刚' ? '' : '前'}</span>
      </div>
    </div>
    {#if !app.mobile}
      <div class="acts">
        <button class="ghost" onclick={hide} data-testid="history-hide">{isHidden ? '取消隐藏' : '隐藏'}</button>
        <button class="primary" onclick={resume} disabled={busy || (!openSid && !node?.online)} data-testid="history-resume">{openSid ? '切换到已打开的会话' : '继续这个对话'}</button>
      </div>
    {/if}
  </header>

  <!-- in the header's shadow, not at the top of a long record the page scrolls away from -->
  {#if busyElsewhere(v.profileId, v.conv, hist.tick)}
    <p class="busy" data-testid="busy-elsewhere">这段对话 {ago(v.conv.updated, hist.tick)}{ago(v.conv.updated, hist.tick) === '刚刚' ? '' : '前'}还有新内容，可能正在别处进行（比如那台电脑自己的终端）。同一段对话两处同时继续，两边都会出问题；请先在那边退出，再在这里继续。</p>
  {/if}

  <div class="body" bind:this={body}>
    <div class="col">
      {#if more}
        <button class="older" onclick={loadOlder} disabled={older} data-testid="history-older">{older ? '正在加载…' : '加载更早的消息'}</button>
      {:else if !loading && messages.length}
        <div class="start">对话开始于 {fullTime(v.conv.created)}</div>
      {/if}
      {#if loading}
        <div class="skeleton"><span class="a"></span><span class="b"></span><span class="a"></span></div>
      {:else if error}
        <p class="err">{error}</p>
      {:else if !messages.length}
        <p class="muted center">这个对话里没有可显示的文字。</p>
      {/if}
      {#each messages as m, i (i)}
        <div class="msg {m.role}" data-testid="history-msg">
          {#if m.role === 'assistant'}<div class="who">{profile?.kind === 'codex' ? 'Codex' : 'Claude'}</div>{/if}
          <div class="bubble">
            {#each parts(m.text) as p}
              {#if p.code}<pre><code>{p.text}</code></pre>{:else}<div class="txt">{p.text}</div>{/if}
            {/each}
            {#if m.tools?.length}
              <div class="tools">{#each m.tools as t}<span class="tool">{t}</span>{/each}</div>
            {/if}
          </div>
          {#if m.time}<div class="time">{clock(m.time)}</div>{/if}
        </div>
      {/each}
    </div>
  </div>

  {#if app.mobile}
    <footer>
      <button onclick={hide} data-testid="history-hide">{isHidden ? '取消隐藏' : '隐藏'}</button>
      <button class="primary" onclick={resume} disabled={busy || (!openSid && !node?.online)} data-testid="history-resume">{openSid ? '切换到已打开的会话' : '继续这个对话'}</button>
    </footer>
  {/if}
</div>

<style>
  .hv { position: absolute; inset: 0; z-index: 5; display: flex; flex-direction: column; background: var(--bg); }
  .hv.mobile { position: fixed; z-index: 17; height: var(--vvh, 100%); }
  header { display: flex; align-items: center; gap: 12px; padding: 12px 16px; border-bottom: 1px solid var(--line); background: var(--panel); flex: none; }
  .mobile header { padding: calc(8px + env(safe-area-inset-top)) 10px 8px; gap: 6px; }
  .back { background: none; border: none; font-size: 22px; line-height: 1; width: 36px; height: 36px; padding: 0; color: var(--dim); flex: none; }
  .back:hover { color: var(--fg); }
  .head { flex: 1; min-width: 0; }
  h2 { margin: 0; font-size: 16px; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .meta { display: flex; flex-wrap: wrap; gap: 4px 12px; margin-top: 4px; font-size: 12px; color: var(--dim); }
  .chip { background: var(--bg); border: 1px solid var(--line); border-radius: 999px; padding: 0 8px; max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .acts { display: flex; gap: 8px; flex: none; }
  .ghost { background: none; color: var(--dim); }
  .ghost:hover { color: var(--fg); }
  .body { flex: 1; overflow-y: auto; min-height: 0; -webkit-overflow-scrolling: touch; }
  .col { max-width: 860px; margin: 0 auto; padding: 18px 16px 28px; display: flex; flex-direction: column; gap: 14px; }
  .mobile .col { padding: 12px 10px 20px; gap: 12px; }
  .busy { margin: 0; padding: 9px 16px; flex: none; border: none; border-bottom: 1px solid rgba(198, 144, 38, .45); background: rgba(198, 144, 38, .1); color: var(--warn); font-size: 13px; line-height: 1.5; }
  .older { align-self: center; background: var(--panel); color: var(--dim); font-size: 12px; border-radius: 999px; padding: 5px 14px; }
  .start { align-self: center; font-size: 12px; color: var(--dim); }
  .center { text-align: center; }
  .msg { display: flex; flex-direction: column; max-width: 88%; }
  .msg.user { align-self: flex-end; align-items: flex-end; }
  .msg.assistant { align-self: flex-start; align-items: flex-start; max-width: 100%; }
  .who { font-size: 11px; color: var(--dim); margin: 0 4px 3px; font-weight: 600; letter-spacing: .02em; }
  .bubble { border-radius: 14px; padding: 9px 13px; line-height: 1.6; word-break: break-word; overflow-wrap: anywhere; display: flex; flex-direction: column; gap: 8px; min-width: 0; max-width: 100%; }
  .user .bubble { background: linear-gradient(135deg, rgba(79, 143, 247, .22), rgba(124, 92, 255, .2)); border: 1px solid rgba(79, 143, 247, .35); border-bottom-right-radius: 4px; }
  .assistant .bubble { background: var(--panel); border: 1px solid var(--line); border-bottom-left-radius: 4px; }
  .txt { white-space: pre-wrap; }
  pre { margin: 0; background: #0b0f14; border: 1px solid var(--line); border-radius: 8px; padding: 9px 11px; overflow-x: auto; font: 12.5px/1.5 "Cascadia Mono", Consolas, ui-monospace, monospace; max-width: 100%; }
  pre code { background: none; padding: 0; word-break: normal; white-space: pre; }
  .tools { display: flex; flex-wrap: wrap; gap: 4px; }
  .tool { font-size: 11px; color: var(--dim); background: var(--bg); border: 1px solid var(--line); border-radius: 999px; padding: 0 8px; font-family: "Cascadia Mono", Consolas, monospace; }
  .time { font-size: 11px; color: var(--dim); margin: 3px 4px 0; }
  .skeleton { display: flex; flex-direction: column; gap: 14px; }
  .skeleton span { height: 54px; border-radius: 14px; background: linear-gradient(90deg, var(--panel), var(--panel-2), var(--panel)); background-size: 200% 100%; animation: shimmer 1.2s infinite; }
  .skeleton .a { width: 70%; } .skeleton .b { width: 45%; align-self: flex-end; }
  @keyframes shimmer { to { background-position: -200% 0; } }
  footer { display: flex; gap: 8px; padding: 8px 10px calc(8px + env(safe-area-inset-bottom)); border-top: 1px solid var(--line); background: var(--panel); flex: none; }
  footer button { padding: 11px 14px; }
  footer .primary { flex: 1; font-size: 15px; }
</style>
