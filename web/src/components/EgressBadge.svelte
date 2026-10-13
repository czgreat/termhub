<script lang="ts">
  import { egressBlocked } from '../lib/egress'
  import { egressClock, useEgressClock } from '../lib/egress-clock.svelte'
  useEgressClock()
  import type { Egress } from '../lib/api'
  let { status }: { status?: Egress } = $props()
  const fresh = $derived(!!status && !egressBlocked(status, egressClock.now))
</script>

{#if status}
  <details class:ok={fresh} class="egress">
    <summary>出口 {fresh ? '正常' : '暂停操作'} · {status.observed_ip || '未知'}</summary>
    <div>{fresh ? status.reason : status.allowed ? '出口结果已过期' : status.reason}</div>
    <div>上面显示的是最近成功心跳确认的 IP，并非实时查询。</div>
    <div>预期：{status.expected_ip}</div>
    <div>确认时间：{status.checked_at ? new Date(status.checked_at * 1000).toLocaleString() : '暂无'}</div>
    <div>异常时保留终端，可查看输出或中止；恢复后需重新发送。</div>
  </details>
{/if}

<style>
  .egress { margin: 3px 8px 7px 20px; font-size: 11px; color: var(--warn, #c08028); overflow-wrap: anywhere; }
  .ok { color: var(--muted); }
  summary { cursor: pointer; min-height: 24px; line-height: 24px; }
  details div { padding: 3px 0; }
</style>
