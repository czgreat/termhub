<script lang="ts">
  // The dot in front of a session's name in tabs, the session list and the
  // phone's top bar (主人 2026-09-25): working, finished and not seen yet, or
  // waiting on an option menu.
  import { statusOf, type Status } from './state.svelte'

  let { sid = '', st }: { sid?: string; st?: Status } = $props()
  const status = $derived(st ?? statusOf(sid))
  const tips: Record<string, string> = { busy: '正在运行', done: '做完了，还没看', ask: '在等你选择或确认' }
</script>

{#if status}<span class="sdot {status}" title={tips[status]} data-testid="status-{status}"></span>{/if}

<style>
  .sdot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-right: 6px; flex: none; vertical-align: middle; position: relative; top: -1px; }
  .busy { background: var(--dim); animation: pulse 1.2s ease-in-out infinite; }
  .done { background: var(--ok); }
  .ask { background: var(--warn); }
  @keyframes pulse { 50% { opacity: .25; } }
  @media (prefers-reduced-motion: reduce) { .busy { animation: none; } }
</style>
