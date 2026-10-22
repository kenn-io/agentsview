<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import { fileReferences } from "../../stores/file-references.svelte.js";
  import { m, getLocale } from "../../i18n/index.js";

  let { sessionId, ordinal, toolUseId = null, eventIndex = null, source = false }: {
    sessionId: string;
    ordinal: number;
    toolUseId?: string | null;
    eventIndex?: number | null;
    source?: boolean;
  } = $props();
  $effect(() => { void fileReferences.load(sessionId); });
  const entry = $derived(fileReferences.entries.get(sessionId));
  const occurrences = $derived(entry?.result?.occurrences.filter((item) =>
    (item.message_ordinal === ordinal || (ordinal === 0 && item.message_ordinal === -1)) && item.tool_use_id === toolUseId && item.event_index === eventIndex,
  ) ?? []);
  const disabled = $derived(entry?.result?.integration === "disabled");
</script>

{#if !disabled && (occurrences.length || entry?.loading || entry?.failed || (source && entry?.result?.source === "unavailable"))}
  <div class="docbank-matches">
    {#if entry?.loading}
      <span class="muted">{m.docbank_checking()}</span>
    {:else if entry?.failed || (source && entry?.result?.source === "unavailable")}
      <div>{m.docbank_unavailable()} <Button size="sm" surface="soft" label={m.shared_retry()} onclick={() => fileReferences.refresh(sessionId)} /></div>
    {/if}
    {#each occurrences as occurrence}
      <div>
        <span class="muted">{m.docbank_image({ n: occurrence.image_index + 1, mediaType: occurrence.media_type, size: occurrence.size === null ? "" : new Intl.NumberFormat(getLocale(), { style: "unit", unit: "byte", unitDisplay: "short" }).format(occurrence.size) })}</span>
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
        {:else}
          {m.docbank_unavailable()} <Button size="sm" surface="soft" label={m.shared_retry()} disabled={entry?.loading} onclick={() => fileReferences.refresh(sessionId)} />
        {/if}
      </div>
    {/each}
  </div>
{/if}

<style>
  .docbank-matches { display: flex; flex-direction: column; gap: 4px; padding: 4px 12px; font-size: 12px; border-top: 1px solid var(--border-subtle); }
  .muted { color: var(--text-muted); }
  a { color: var(--accent-blue); }
</style>
