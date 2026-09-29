<script lang="ts">
  import { app } from '../lib/state.svelte'
  import { resolveRecovery } from '../lib/composer'
  let { sid }: { sid: string } = $props()
</script>

{#if app.recovery[sid]}
  <div class="recovery">
    {app.sending[sid] ? '正在等待发送确认…' : '上次发送未完成或结果待确认。检查终端后再处理副本，避免重复提交。'}
    <textarea readonly value={app.recovery[sid]} aria-label="发送保留副本"></textarea>
    <button disabled={!!app.sending[sid]} onclick={() => { if (confirm('确认终端里没有这段正文，需要放回草稿重新编辑？')) resolveRecovery(sid, true) }}>放回草稿</button>
    <button disabled={!!app.sending[sid]} onclick={() => { if (confirm('已检查终端，确认不再需要这份副本？')) resolveRecovery(sid, false) }}>清除副本</button>
  </div>
{/if}

<style>
  .recovery { padding: 8px; background: #4d3a12; }
  textarea { display: block; width: 100%; max-height: 100px; font-size: 16px; }
</style>
