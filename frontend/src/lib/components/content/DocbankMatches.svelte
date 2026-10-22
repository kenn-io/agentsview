<script lang="ts">
  import { fileReferences } from "../../stores/file-references.svelte.js";
  import { m, getLocale } from "../../i18n/index.js";

  let { sessionId = "", ordinal = 0, toolUseId = null, eventIndex = null, callIndex = 0 }: {
    sessionId?: string;
    ordinal?: number;
    toolUseId?: string | null;
    callIndex?: number;
    eventIndex?: number | null;
  } = $props();
  const occurrences = $derived(toolUseId === null
    ? fileReferences.forMessage(sessionId, ordinal)
    : fileReferences.forTool(sessionId, ordinal, toolUseId, eventIndex, callIndex));
</script>

{#if occurrences.length}
  <div class="docbank-matches">
    {#each occurrences as occurrence}
      <div>
        <div class="muted">{occurrence.size === null ? m.docbank_file_without_size({ n: occurrence.image_index + 1, mediaType: occurrence.media_type }) : m.docbank_file({ n: occurrence.image_index + 1, mediaType: occurrence.media_type, size: new Intl.NumberFormat(getLocale(), { style: "unit", unit: "byte", unitDisplay: "short" }).format(occurrence.size) })}</div>
        <div>
          {#if occurrence.state === "found"}
            {#each occurrence.matches as match}
              {@const state = match.state === "current" ? m.docbank_current() : match.state === "prior" ? m.docbank_prior() : m.docbank_trash()}
              <div>
                {#if match.open_url}
                  <a href={match.open_url} target="_blank" rel="noreferrer">{state} · {match.path || match.name}</a>
                {:else}
                  <span>{state} · {match.path || match.name}</span>
                {/if}
              </div>
            {/each}
            {#if occurrence.match_total > occurrence.matches.length}
              <span class="muted">{m.docbank_more({ count: occurrence.match_total - occurrence.matches.length })}</span>
            {/if}
          {:else if occurrence.state === "not_found"}
            {m.docbank_not_found()}
          {:else if occurrence.state === "limit_exceeded"}
            {m.docbank_limit_exceeded()}
          {:else}
            {m.docbank_unavailable()}
          {/if}
        </div>
      </div>
    {/each}
  </div>
{/if}

<style>
  .docbank-matches { display: flex; flex-direction: column; gap: 4px; padding: 4px 12px; font-size: 12px; border-top: 1px solid var(--border-subtle); }
  .muted { color: var(--text-muted); }
  a { color: var(--accent-blue); }
</style>
