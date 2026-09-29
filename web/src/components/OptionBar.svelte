<script lang="ts">
  // The option cards for a menu on the screen (docs/M11 第 6 节 "选项条"):
  // one tap picks, or toggles a checkbox; "提交" sends the Enter of a
  // multi-select. Never takes the focus, so the soft keyboard stays put.
  import { app } from '../lib/state.svelte'
  import { terms } from '../lib/terms'
  import { keysToPick, keysToSubmit, splitKeys } from '../lib/menu'

  let { sid }: { sid: string } = $props()
  const menu = $derived(app.menus[sid])

  // keys go one at a time, 40 ms apart (see splitKeys)
  let queue: Promise<void> = Promise.resolve()
  function send(keys: string) {
    const t = terms.get(sid)
    if (!t) return
    queue = queue.then(async () => { for (const k of splitKeys(keys)) { t.input(k); await new Promise(r => setTimeout(r, 40)) } })
  }
  function pick(i: number) { const t = terms.get(sid); if (t && menu) send(keysToPick(menu, i, t.appCursor())) }
  // Claude Code follows a question's Submit with a review screen ("Ready to
  // submit your answers? 1. Submit answers 2. Cancel"); "提交" means the
  // answer goes, so that screen is confirmed too when it comes up within a
  // few seconds (seen with a real CLI, 09-24).
  let confirmUntil = 0
  function submit() {
    const t = terms.get(sid)
    if (!t || !menu) return
    confirmUntil = Date.now() + 6000
    send(keysToSubmit(menu, t.appCursor()))
  }
  // A card acts on release, not on press (docs/M11 第 6 节): a finger that
  // starts scrolling the strip must not pick anything, since Claude Code
  // confirms a numbered choice at once. The press still has its default
  // prevented so the focus and soft keyboard stay put. A click with no
  // pointer behind it (detail 0: keyboard Enter/Space) acts too.
  const SLOP = 8
  let press: { id: number; x: number; y: number; el: EventTarget | null } | null = null
  const far = (e: PointerEvent) => !press || Math.hypot(e.clientX - press.x, e.clientY - press.y) > SLOP
  function tap(act: () => void) {
    return {
      onpointerdown: (e: PointerEvent) => { e.preventDefault(); press = e.button === 0 ? { id: e.pointerId, x: e.clientX, y: e.clientY, el: e.currentTarget } : null },
      onpointermove: (e: PointerEvent) => { if (press?.id === e.pointerId && far(e)) press = null },
      onpointerup: (e: PointerEvent) => { const ok = press?.id === e.pointerId && press.el === e.currentTarget && !far(e); press = null; if (ok) act() },
      onpointercancel: () => { press = null },
      onclick: (e: MouseEvent) => { if (e.detail === 0) act() },
    }
  }
  $effect(() => {
    const m = menu
    if (!m || Date.now() > confirmUntil) return
    const it = m.items.find(i => /^Submit answers?$/i.test(i.label.trim()))
    if (it?.number) { confirmUntil = 0; send(String(it.number)) }
  })
</script>

{#if menu}
  <div class="options" data-testid="options" data-kind={menu.kind}>
    {#if menu.title}<div class="title muted" data-testid="option-title">{menu.title}</div>{/if}
    {#each menu.items as it, i}
      <button type="button" class:cursor={i === menu.cursor} class:checked={it.checked} data-testid="option"
        {...tap(() => pick(i))}>
        {#if menu.kind === 'multi'}<span class="box">{it.checked ? '☑' : '☐'}</span>{/if}
        {#if it.number}<span class="num">{it.number}</span>{/if}
        {it.label}
      </button>
    {/each}
    {#if menu.kind === 'multi'}
      <button type="button" class="primary submit" data-testid="option-submit" {...tap(submit)}>提交</button>
    {/if}
    {#each menu.extras ?? [] as ex (ex.label)}
      <button type="button" class="extra" data-testid="option-extra" {...tap(() => send(ex.keys))}>{ex.label}</button>
    {/each}
  </div>
{/if}

<style>
  .options { position: absolute; left: 0; right: 0; bottom: 0; z-index: 5; display: flex; flex-wrap: wrap; gap: 6px; padding: 6px; background: var(--panel); border-top: 1px solid var(--line); max-height: 45%; overflow-y: auto; box-shadow: 0 -6px 18px rgba(0,0,0,.35); }
  button { text-align: left; padding: 6px 10px; font-size: 14px; max-width: 100%; white-space: normal; user-select: none; -webkit-user-select: none; }
  button.cursor { border-color: var(--accent); box-shadow: inset 0 0 0 1px var(--accent); }
  button.checked { background: #1f3a5f; }
  .num { display: inline-block; min-width: 1.2em; margin-right: 6px; color: var(--accent); font-weight: 600; }
  .box { margin-right: 6px; }
  .submit { margin-left: auto; }
  .extra { color: var(--dim); border-style: dashed; }
  .title { width: 100%; font-size: 13px; padding: 0 2px; }
</style>
