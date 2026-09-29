<script lang="ts">
  // The input box (docs/M11 第 5 节): write with any input method, then the
  // whole text goes into the terminal through the paste path followed by a
  // carriage return, so multi-line text is submitted once, not line by line.
  // The draft is kept per session. A long press sends Esc first: Claude Code
  // in a terminal knows only Enter (queue) and Esc (interrupt), so "interrupt
  // and send" is Esc, then the text, then Enter.
  import { app, saveDrafts } from '../lib/state.svelte'
  import { sendDraft } from '../lib/composer'
  import RecoveryCopy from './RecoveryCopy.svelte'

  let { sid }: { sid: string } = $props()
  let box: HTMLTextAreaElement
  let pressTimer: number | undefined
  let longPressed = $state(false)

  async function send(interrupt = false) {
    if (!await sendDraft(sid, interrupt)) return // sid: the session of the press, even if another is shown meanwhile
    // On a phone the keyboard goes away after sending, so the reply is in view;
    // on a desktop the box keeps the focus for the next message.
    if (app.mobile) box.blur()
    else box.focus()
  }
  function keydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); send() }
  }
  function pressStart(e: PointerEvent) {
    e.preventDefault() // keep the keyboard as it is
    longPressed = false
    pressTimer = window.setTimeout(() => { longPressed = true; navigator.vibrate?.(30); send(true) }, 550)
  }
  function pressEnd() {
    clearTimeout(pressTimer)
    if (!longPressed) send()
    longPressed = false
  }
</script>

<RecoveryCopy {sid} />
<div class="composer">
  <textarea bind:this={box} bind:value={app.drafts[sid]} rows="2" placeholder="输入…" data-testid="composer"
    autocapitalize="off" autocomplete="off" spellcheck="false" onkeydown={keydown} oninput={() => saveDrafts()}></textarea>
  <button type="button" class="primary" data-testid="send-enter" title="整段写入终端并回车（Ctrl+Enter）；空着点就是单独一个回车；长按 = 先按 Esc 打断当前工作，再发送"
    onpointerdown={pressStart} onpointerup={pressEnd} onpointercancel={() => clearTimeout(pressTimer)} oncontextmenu={e => e.preventDefault()}>发送</button>
</div>

<style>
  .composer { display: flex; gap: 6px; padding: 6px; background: var(--panel); border-top: 1px solid var(--line); flex: none; align-items: stretch; }
  textarea { flex: 1; resize: none; font-size: 16px; /* under 16px iOS zooms the page on focus */ line-height: 1.3; min-height: 0; }
  button { padding: 0 14px; white-space: nowrap; user-select: none; -webkit-user-select: none; touch-action: none; }
</style>
