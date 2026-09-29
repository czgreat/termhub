<script lang="ts">
  // The key bar for touch screens (docs/M11 第 6 节): keys a soft keyboard
  // lacks, sticky Ctrl/Alt, arrows that repeat while held, paste and image.
  // Pressing a key must not move the focus or toggle the soft keyboard, so
  // pointerdown has its default prevented. The bar is wider than a phone and
  // scrolls sideways, so a key acts on release, and only if the finger moved
  // less than 8 px: a swipe that starts on Esc must not interrupt Claude.
  import { app, tapMod, focusedSid } from '../lib/state.svelte'
  import { terms } from '../lib/terms'

  type Key = { label: string; seq?: string; arrow?: string; mod?: 'ctrl' | 'alt'; action?: 'paste' | 'image'; title?: string; wide?: boolean }
  const first: Key[] = [
    { label: 'Esc', seq: '\x1b' }, { label: 'Tab', seq: '\t' }, { label: 'Ctrl', mod: 'ctrl' }, { label: 'Alt', mod: 'alt' },
    { label: '⇧Tab', seq: '\x1b[Z', title: 'Shift+Tab' },
    { label: '←', arrow: 'D' }, { label: '↑', arrow: 'A' }, { label: '↓', arrow: 'B' }, { label: '→', arrow: 'C' },
    { label: '^C', seq: '\x03', title: 'Ctrl+C' }, { label: '⏎', seq: '\r', title: 'Enter' },
    { label: '粘贴', action: 'paste' }, { label: '图片', action: 'image' },
  ]
  const second: Key[] = [
    { label: 'Home', seq: '\x1b[H' }, { label: 'End', seq: '\x1b[F' }, { label: 'PgUp', seq: '\x1b[5~' }, { label: 'PgDn', seq: '\x1b[6~' },
    { label: '/', seq: '/' }, { label: '|', seq: '|' }, { label: '~', seq: '~' }, { label: '-', seq: '-' }, { label: '\\', seq: '\\' },
  ]
  let fileInput: HTMLInputElement

  const term = () => terms.get(focusedSid())

  function send(k: Key) {
    const t = term()
    if (k.mod) { tapMod(k.mod); return }
    if (k.action === 'paste') { navigator.clipboard?.readText().then(s => { if (s) paste(s) }).catch(() => {}); return }
    if (k.action === 'image') { fileInput.click(); return }
    if (!t) return
    if (k.arrow) t.input((t.appCursor() ? '\x1bO' : '\x1b[') + k.arrow)
    else if (k.seq) t.input(k.seq)
  }
  // In the 输入框 mode the terminal is read-only and all text goes through
  // the box (docs/M11 第 5 节), so pasted text lands in the draft.
  function paste(s: string) {
    const sid = focusedSid()
    if (app.inputMode === 'box' && sid) app.drafts[sid] = (app.drafts[sid] ?? '') + s
    else term()?.paste(s)
  }

  // One press at a time. A held arrow starts repeating after 400 ms (that
  // still begins on press); moving away cancels it and sends nothing.
  const SLOP = 8
  type Press = { id: number; x: number; y: number; k: Key; start?: number; repeat?: number; repeated?: boolean }
  let press: Press | null = null
  // paste/image wait for the click that follows the release: browsers take
  // a click, not a touch pointerdown, as the user activation that clipboard
  // reads and the file dialog need.
  let armed: Key | null = null
  const far = (p: Press, e: PointerEvent) => Math.hypot(e.clientX - p.x, e.clientY - p.y) > SLOP
  function end() {
    if (press) { clearTimeout(press.start); clearInterval(press.repeat) }
    press = null
  }
  function down(e: PointerEvent, k: Key) {
    e.preventDefault() // keep the focus (and the soft keyboard) where it is
    if (e.button !== 0) return
    end(); armed = null
    // a mouse dragged off the key still reports its release here (touch is captured anyway)
    try { (e.currentTarget as Element).setPointerCapture(e.pointerId) } catch {}
    const p: Press = press = { id: e.pointerId, x: e.clientX, y: e.clientY, k }
    if (k.arrow) p.start = window.setTimeout(() => { p.repeated = true; send(k); p.repeat = window.setInterval(() => send(k), 80) }, 400)
  }
  function move(e: PointerEvent) { if (press?.id === e.pointerId && far(press, e)) end() }
  function up(e: PointerEvent) {
    const p = press
    if (!p || p.id !== e.pointerId) return
    const tap = !far(p, e) && !p.repeated
    end()
    if (!tap) return
    if (p.k.action) armed = p.k
    else send(p.k)
  }
  function click(e: MouseEvent, k: Key) {
    // detail 0: a click with no pointer behind it (keyboard Enter/Space)
    if (e.detail === 0 || (k.action && armed === k)) { armed = null; send(k) }
  }

  // No safe-area padding while the soft keyboard is up: the bar then sits on
  // the keyboard, not on the home indicator (docs/M11 第 4 节). Reads the
  // visual viewport directly, the same number App.svelte puts in --vvh.
  let kbOpen = $state(false)
  $effect(() => {
    const vv = window.visualViewport
    if (!vv) return
    const check = () => { kbOpen = window.innerHeight - vv.height > 120 }
    vv.addEventListener('resize', check)
    check()
    return () => vv.removeEventListener('resize', check)
  })
  $effect(() => end)

  function picked(e: Event) {
    const files = Array.from((e.target as HTMLInputElement).files ?? [])
    ;(e.target as HTMLInputElement).value = ''
    if (files.length) term()?.files(files)
  }
</script>

<div class="keybar" class:kb={kbOpen} data-testid="keybar">
  {#each [...first, ...second] as k (k.label)}
    <button type="button" data-key={k.label} title={k.title ?? k.label}
      class:once={k.mod && app.keymod[k.mod] === 'once'} class:locked={k.mod && app.keymod[k.mod] === 'locked'}
      onpointerdown={e => down(e, k)} onpointermove={move} onpointerup={up} onpointercancel={end}
      onclick={e => click(e, k)} oncontextmenu={e => e.preventDefault()}>{k.label}</button>
  {/each}
  <input type="file" accept="image/*" bind:this={fileInput} onchange={picked} style="display:none" />
</div>

<style>
  .keybar { display: flex; gap: 4px; padding: 4px 6px calc(4px + env(safe-area-inset-bottom)); overflow-x: auto; background: var(--panel); border-top: 1px solid var(--line); flex: none; scrollbar-width: none; touch-action: pan-x; }
  .keybar.kb { padding-bottom: 4px; }
  .keybar::-webkit-scrollbar { display: none; }
  button { flex: none; min-width: 40px; height: 34px; padding: 0 8px; font-size: 13px; border-radius: 6px; user-select: none; -webkit-user-select: none; touch-action: pan-x; }
  button.once { background: #2b4a75; border-color: var(--accent); }
  button.locked { background: var(--accent); border-color: var(--accent); color: #fff; }
</style>
