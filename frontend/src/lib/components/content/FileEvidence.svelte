<script lang="ts">
  import type { ServiceLocalFileEvidence, IntegrationsdocbankMatch } from "../../api/generated/index.js";
  import type { SessionFileReferencesStore } from "../../stores/file-references.svelte.js";
  import { Button } from "@kenn-io/kit-ui";
  import { loadAssetImages } from "../../utils/markdown.js";
  import { m } from "../../i18n/index.js";
  let { records, store }: { records: ServiceLocalFileEvidence[]; store?: SessionFileReferencesStore } = $props();
  let expanded = $state(new Set<string>());
  const stateLabel = (state: string) => state === "trash" ? m.docbank_trash() : state === "prior" ? m.docbank_prior() : m.docbank_current();
</script>

{#each records as record (record.occurrence_id)}
  {@const remote = store?.occurrences.get(record.occurrence_id)}
  {@const enabled = store && store.integration !== "" && store.integration !== "disabled"}
  <div class="file-evidence" data-occurrence={record.occurrence_id} data-block={record.block_index} data-result={record.result_event_index}>
    {#if record.asset_ref.startsWith("asset://") && ["image/png", "image/jpeg", "image/webp", "image/gif"].includes(record.media_type)}
      <div use:loadAssetImages={record.asset_ref}><img src={`/__agentsview_asset__/${encodeURIComponent(record.asset_ref.slice(8))}`} alt={m.docbank_image_alt({ name: record.display_name || m.docbank_attachment() })} /></div>
    {/if}
    <span>{record.display_name || record.display_path || m.docbank_attachment()}</span>
    {#if record.media_type}<span class="file-type">{record.media_type}</span>{/if}
    {#if enabled}
      {#if record.ineligible_reason || remote?.state === "ineligible"}<span>{m.docbank_exact_unavailable()}</span>
      {:else if remote?.state === "not_found"}<span>{m.docbank_not_found()}</span>{/if}
      {#if remote?.matches.length}
        {@render matchRow(remote.matches[0]!)}
        {#if remote.matches.length > 1}
          <details open={expanded.has(record.occurrence_id)}>
            <summary>{m.docbank_matches()}</summary>
            {#each remote.matches.slice(1) as match (`${match.node_id}/${match.version_id}`)}{@render matchRow(match)}{/each}
          </details>
        {/if}
      {/if}
      {#if remote?.next_reference_offset != null}
        <Button size="sm" surface="soft" label={remote.state === "deferred" || remote.state === "unavailable" ? m.docbank_retry_matches() : m.docbank_more_matches()} disabled={store?.busy} onclick={() => { expanded = new Set([...expanded, record.occurrence_id]); void store?.loadMore(record.occurrence_id); }} />
      {/if}
      {#if record.media_type.startsWith("image/")}<small>{m.docbank_image_details()}</small>
      {:else if record.proof_method.startsWith("pi_native_") || record.kind === "read" || record.kind === "write"}<small>{m.docbank_pi_details()}</small>{/if}
    {/if}
  </div>
{/each}

{#snippet matchRow(match: IntegrationsdocbankMatch)}
  <div class="file-match">
    {#if match.open_url}
      <a href={match.open_url} target="_blank" rel="noreferrer" aria-label={m.docbank_match({ state: stateLabel(match.state), name: match.path || match.name })}>{m.docbank_match({ state: stateLabel(match.state), name: match.path || match.name })}</a>
    {:else}<span>{m.docbank_match({ state: stateLabel(match.state), name: match.path || match.name })} · {match.node_id} / {match.version_id}</span>{/if}
  </div>
{/snippet}

<style>
  .file-evidence { font-size: 12px; padding: 4px 12px; overflow-wrap: anywhere; }
  .file-type, small { color: var(--text-muted); margin-inline-start: 6px; }
  .file-match { display: flex; gap: 8px; }
  a { color: var(--accent-blue); }
  img { max-width: 100%; max-height: 320px; object-fit: contain; }
</style>
