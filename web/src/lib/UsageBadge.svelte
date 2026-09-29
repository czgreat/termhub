<script lang="ts">
  // A session's "$3.42 · 38%" (lib/usage.ts); nothing when unknown.
  import { app } from './state.svelte'
  import { usageText, usageTip } from './usage'

  let { sid }: { sid: string } = $props()
  const u = $derived(app.usage[sid])
  const text = $derived(usageText(u))
</script>

{#if text}<span class="usage" title={usageTip(u)} data-testid="usage">{text}</span>{/if}

<style>
  .usage { font-size: 11px; color: var(--dim); white-space: nowrap; font-variant-numeric: tabular-nums; }
</style>
