<script lang="ts">
  import { Button, SearchByMeaningHint, SearchByMeaningRow, type SearchByMeaningState } from "@kenn-io/kit-ui";
  import { EmbeddingsService, type VectorBuildStatus } from "../../api/generated/index.js";
  import { ApiError } from "../../api/runtime.js";
  import { getLocale, m } from "../../i18n/index.js";

  let { status, hint = false, onchange = () => {} }: {
    status: VectorBuildStatus | null;
    hint?: boolean;
    onchange?: () => void;
  } = $props();
  let error = $state<string | null>(null);
  const setup = $derived(status?.setup);
  const rowState = $derived.by((): SearchByMeaningState => {
    if (setup?.busy || setup?.installing) return { kind: "downloading", doneBytes: setup?.downloaded_bytes ?? 0, totalBytes: setup?.total_bytes ?? 0 };
    if (status?.running) return { kind: "indexing", done: status.done, total: status.total, etaMs: status.estimate_ready ? status.eta_milliseconds : undefined };
    if (setup?.action === "turn-on" || !setup?.enabled) return setup?.missing_bytes && setup.provider === "built-in" ? { kind: "off", downloadBytes: setup.missing_bytes } : { kind: "downloaded" };
    if (setup.provider === "ollama") return { kind: "ollama" };
    return { kind: "on" };
  });
  // Turn on approves only the download the row or hint shows; the server declines a larger one.
  const shownBytes = $derived(setup?.provider === "built-in" ? (setup.missing_bytes ?? 0) : 0);
  const downloadLabel = $derived(m.meaning_download({ size: new Intl.NumberFormat(getLocale(), { style: "unit", unit: "megabyte", maximumFractionDigits: 0 }).format((setup?.missing_bytes ?? 0) / 1000000) }));
  const labels = $derived({
    name: m.meaning_name(), off: m.meaning_off(),
    on: m.meaning_on(),
    ollama: m.meaning_ollama(),
    turningOn: m.meaning_turning_on(), turnOn: m.meaning_turn_on(),
    download: m.meaning_download({ size: "{size}" }), downloaded: setup?.provider === "ollama" ? m.meaning_uses_ollama() : setup?.provider === "configured" ? m.meaning_configured() : m.meaning_downloaded(),
    // An Ollama setup prints no line: nothing switches providers.
    keepsIndex: "", downloading: setup?.total_bytes ? m.meaning_downloading({ done: "{done}", total: "{total}" }) : m.meaning_turning_on(),
    indexing: m.meaning_indexing({ done: "{done}", total: "{total}", items: "{items}" }), timeLeft: m.meaning_time_left({ time: "{time}" }), meanwhile: m.meaning_meanwhile(),
    underMinute: m.meaning_under_minute(), aboutTime: m.meaning_about_time({ time: "{time}" }), progress: m.meaning_progress(), hint: setup?.provider === "built-in" && setup.missing_bytes > 0 ? `${m.meaning_hint()} · ${downloadLabel}` : m.meaning_hint(),
  });
  async function activate() {
    if (setup?.action !== "turn-on") return;
    error = null;
    try {
      await EmbeddingsService.postApiV1EmbeddingsSetup({ approved_bytes: shownBytes }).catch((e) => {
        if (!(e instanceof ApiError && e.status === 409)) throw e;
      });
      onchange();
    } catch (e) {
      error = e instanceof Error ? e.message : m.settings_embeddings_load_failed();
    }
  }
</script>

{#if setup}
  {#if hint}
    {#if setup.action === "turn-on"}
      <SearchByMeaningHint {labels} onturnon={() => void activate()} />
    {/if}
  {:else}
    <SearchByMeaningRow state={rowState} {labels} readonly={!setup.action || !!setup.error} itemNoun={m.meaning_chunks({ count: status?.total ?? 0 })} onturnon={() => void activate()} onusebuiltin={() => {}} />
  {/if}
  {#if error || setup.error}
    <div class="setup-error">
      <p role="alert">{error || setup.error}</p>
      {#if setup.error && setup.action}
        <Button size="sm" label={m.shared_retry()} onclick={() => void activate()} />
      {/if}
    </div>
  {/if}
{/if}

<style>
  .setup-error {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-3);
  }
  .setup-error p {
    margin: 0;
  }
</style>
