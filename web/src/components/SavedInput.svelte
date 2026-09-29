<script lang="ts">
  import { app, savedInputSids, sessionOf, saveDrafts } from '../lib/state.svelte'
  import RecoveryCopy from './RecoveryCopy.svelte'
  let editing = $state('')
  const sids = $derived([...new Set([...savedInputSids(), ...(editing ? [editing] : [])])])
</script>

<!-- G4: independent of running sessions, open tabs and phone input mode.
     Restoring an ended session's copy leaves an editable, reachable draft. -->
{#if sids.length}
  <details data-testid="saved-input">
    <summary>草稿与保留副本（{sids.length}）</summary>
    {#each sids as sid (sid)}
      <details class="entry">
        <summary>{sessionOf(sid)?.title || sessionOf(sid)?.profile_name || sid}</summary>
        <RecoveryCopy {sid} />
        {#if app.drafts[sid] || editing === sid}
          <label>草稿<textarea aria-label="保存的草稿" value={app.drafts[sid]} onfocus={() => editing = sid} onblur={() => editing = ''} oninput={e => { app.drafts[sid] = e.currentTarget.value; saveDrafts(true) }}></textarea></label>
          <button onclick={() => { if (confirm('确认删除这份草稿？')) { delete app.drafts[sid]; saveDrafts(true) } }}>删除草稿</button>
        {/if}
      </details>
    {/each}
  </details>
{/if}

<style>
  summary { cursor: pointer; overflow-wrap: anywhere; }
  .entry { margin-top: 8px; }
  textarea { display: block; width: 100%; max-height: 150px; font-size: 16px; }
</style>
