<script lang="ts">
  import { Chip, type ChipTone } from "@kenn-io/kit-ui";
  import { onMount } from "svelte";
  import { getLocale, m } from "../../i18n/index.js";
  import { EmbeddingsService } from "../../api/generated/index";
  import type { VectorBuildStatus, VectorGenerationInfo } from "../../api/generated/index";
  import {
    ApiError,
    isAbortError,
  } from "../../api/runtime.js";
  import { LatestRead } from "../../utils/latest-read.js";

  import SearchByMeaningControl from "../shared/SearchByMeaningControl.svelte";

  // Poll fast only while a build is actually running; when idle the section
  // only needs to notice an externally started build (CLI, scheduler)
  // eventually. Hidden pages do not poll at all.
  const ACTIVE_POLL_MS = 2000;
  const IDLE_POLL_MS = 60_000;
  // During a build, generation coverage moves with the build; refresh the
  // rows on a slower multiple of the active status poll.
  const GENERATIONS_EVERY_ACTIVE_POLLS = 5;

  let status = $state<VectorBuildStatus | null>(null);
  let generations = $state<VectorGenerationInfo[]>([]);
  let unavailableReason = $state<string | null>(null);
  let loadError = $state<string | null>(null);
  let loaded: boolean = $state(false);
  let elapsedMs: number | null = $state(null);

  let timer: ReturnType<typeof setTimeout> | null = null;
  let disposed = false;
  let activePollsSinceGenerations = 0;
  const statusRead = new LatestRead();
  const generationsRead = new LatestRead();

  const numberFormat = $derived(new Intl.NumberFormat(getLocale()));
  const running = $derived(status?.running ?? false);
  const settingUp = $derived(status?.setup?.busy || status?.setup?.installing);
  const phase = $derived(status?.phase ?? "");
  const estimateReady = $derived(status?.estimate_ready ?? false);
  const ratePerSecond = $derived(status?.rate_per_second ?? null);
  function clearTimer(): void {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
  }

  function schedule(): void {
    clearTimer();
    if (disposed || unavailableReason !== null || document.hidden) return;
    const interval = running || settingUp ? ACTIVE_POLL_MS : IDLE_POLL_MS;
    timer = setTimeout(() => void poll(), interval);
  }

  async function poll(): Promise<void> {
    let withGenerations = true;
    if (running) {
      activePollsSinceGenerations += 1;
      withGenerations =
        activePollsSinceGenerations >= GENERATIONS_EVERY_ACTIVE_POLLS;
    }
    await refresh(withGenerations);
    schedule();
  }

  async function refresh(withGenerations: boolean): Promise<void> {
    const signal = statusRead.begin();
    try {
      const next = await EmbeddingsService.getApiV1EmbeddingsStatus({}, { signal });
      if (disposed || !statusRead.isCurrent(signal)) return;
      const wasRunning = status?.running ?? false;
      status = next;
      unavailableReason = null;
      loadError = null;
      updateElapsed(next);
      if (next.setup?.enabled !== false && next.setup?.action !== "turn-on" && (withGenerations || (wasRunning && !next.running))) {
        activePollsSinceGenerations = 0;
        await refreshGenerations();
      }
    } catch (e) {
      if (
        disposed ||
        isAbortError(e) ||
        !statusRead.isCurrent(signal)
      ) return;
      elapsedMs = null;
      if (e instanceof ApiError && e.status === 501) {
        unavailableReason = e.message;
        status = null;
        generations = [];
        loadError = null;
      } else {
        loadError =
          e instanceof Error && e.message
            ? e.message
            : m.settings_embeddings_load_failed();
      }
    } finally {
      if (statusRead.finish(signal)) loaded = true;
    }
  }

  async function refreshGenerations(): Promise<void> {
    const signal = generationsRead.begin();
    try {
      const res = await EmbeddingsService.getApiV1EmbeddingsGenerations({}, { signal });
      if (disposed || !generationsRead.isCurrent(signal)) return;
      generations = res.generations ?? [];
    } finally {
      generationsRead.finish(signal);
    }
  }

  function updateElapsed(s: VectorBuildStatus): void {
    if (!s.running) {
      elapsedMs = null;
      return;
    }
    elapsedMs = s.started_at
      ? Math.max(0, Date.now() - Date.parse(s.started_at))
      : null;
  }

  function onVisibilityChange(): void {
    if (document.hidden) {
      clearTimer();
      statusRead.cancel();
      generationsRead.cancel();
      return;
    }
    clearTimer();
    void poll();
  }

  onMount(() => {
    void refresh(true).then(schedule);
    document.addEventListener("visibilitychange", onVisibilityChange);
    return () => {
      disposed = true;
      statusRead.cancel();
      generationsRead.cancel();
      clearTimer();
      document.removeEventListener("visibilitychange", onVisibilityChange);
    };
  });

  function formatDurationLocalized(ms: number): string {
    const totalSeconds = Math.max(0, Math.round(ms / 1000));
    const hours = Math.floor(totalSeconds / 3600);
    const minutes = Math.floor((totalSeconds % 3600) / 60);
    const seconds = totalSeconds % 60;
    if (hours > 0) {
      return m.settings_embeddings_duration_hours_minutes({ hours, minutes });
    }
    if (minutes > 0) {
      return m.settings_embeddings_duration_minutes_seconds({
        minutes,
        seconds,
      });
    }
    return m.settings_embeddings_duration_seconds({ seconds });
  }

  function stateLabel(state: string): string {
    if (state === "active") return m.settings_embeddings_state_active();
    if (state === "building") return m.settings_embeddings_state_building();
    if (state === "retired") return m.settings_embeddings_state_retired();
    return state;
  }

  function stateTone(state: string): ChipTone {
    if (state === "active") return "success";
    if (state === "building") return "warning";
    if (state === "failed") return "danger";
    return "muted";
  }
</script>

<div class="embeddings-settings">
  <SearchByMeaningControl {status} onchange={() => void refresh(true).then(schedule)} />
  {#if !loaded}
    <p class="muted">{m.settings_embeddings_loading()}</p>
  {:else if unavailableReason !== null}
    <p class="muted">{m.settings_embeddings_unavailable()}</p>
    <p class="muted detail">{unavailableReason}</p>
  {:else if loadError !== null}
    <p class="msg error" role="alert">{loadError}</p>
  {:else if status && status.setup?.enabled !== false && status.setup?.action !== "turn-on" && !status.setup?.busy}
    {#if running}
      {#if phase === "scanning" || status.total <= 0}
        <p class="muted" aria-live="polite">
          {m.settings_embeddings_scanning_hint()}
        </p>
      {:else if (estimateReady && ratePerSecond !== null) || elapsedMs !== null}
        <div class="progress-block" aria-live="polite">
          <div class="progress-stats">
            {#if estimateReady && ratePerSecond !== null}
              <span>
                {m.settings_embeddings_rate({
                  count: Math.max(1, Math.round(ratePerSecond)),
                  rateLabel: numberFormat.format(Math.round(ratePerSecond)),
                })}
              </span>
            {/if}
            {#if elapsedMs !== null}
              <span>
                {m.settings_embeddings_elapsed({
                  duration: formatDurationLocalized(elapsedMs),
                })}
              </span>
            {/if}
          </div>
        </div>
      {/if}
    {/if}

    {#if status.model || status.dimension}
      <div class="status-grid">
        {#if status.model}
          <span class="row-label">{m.settings_embeddings_model_label()}</span>
          <span class="row-value mono">{status.model}</span>
        {/if}
        {#if status.dimension}
          <span class="row-label">{m.settings_embeddings_dimension_label()}</span>
          <span class="row-value">{numberFormat.format(status.dimension)}</span>
        {/if}
      </div>
    {/if}

    {#if !running && status.last_error}
      <p class="msg error" role="alert">{status.last_error}</p>
    {:else if !running && status.last_result}
      <div class="status-grid">
        <span class="row-label">{m.settings_embeddings_last_build_label()}</span
        >
        <span class="row-value">
          {m.settings_embeddings_last_build_completed()}
          {#if status.last_result.Activated}
            · {m.settings_embeddings_last_build_activated()}
          {/if}
        </span>
        <span class="row-label"
          >{m.settings_embeddings_last_build_documents()}</span
        >
        <span class="row-value"
          >{numberFormat.format(status.last_result.Fill.Documents)}</span
        >
        <span class="row-label">{m.settings_embeddings_last_build_chunks()}</span
        >
        <span class="row-value"
          >{numberFormat.format(status.last_result.Fill.Chunks)}</span
        >
      </div>
    {/if}

    <div class="subsection">
      <h4 class="subsection-title">
        {m.settings_embeddings_generations_title()}
      </h4>
      {#if generations.length === 0}
        <p class="muted">{m.settings_embeddings_generations_empty()}</p>
      {:else}
        <ul class="generation-list">
          {#each generations as gen (gen.id)}
            <li class="generation-row">
              <Chip size="xs" tone={stateTone(gen.state)}>
                {stateLabel(gen.state)}
              </Chip>
              <span class="gen-model mono">{gen.model} · {numberFormat.format(gen.dimension)}d</span>
              <span class="gen-coverage">
                {m.settings_embeddings_coverage({
                  embeddedLabel: numberFormat.format(gen.embedded),
                  missingLabel: numberFormat.format(gen.missing),
                })}
              </span>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/if}
</div>

<style>
  .embeddings-settings {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }

  .muted {
    font-size: 12px;
    color: var(--text-muted);
    margin: 0;
  }

  .muted.detail {
    font-family: var(--font-mono, monospace);
    font-size: 11px;
  }

  .msg {
    font-size: 11px;
    margin: 0;
  }

  .msg.error {
    color: var(--accent-red, #ef4444);
  }

  .status-grid {
    display: grid;
    grid-template-columns: auto 1fr;
    column-gap: 16px;
    row-gap: 6px;
    align-items: center;
  }

  .row-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
  }

  .row-value {
    font-size: 12px;
    color: var(--text-primary);
  }

  .mono {
    font-family: var(--font-mono, monospace);
  }

  .progress-block {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .progress-stats {
    display: flex;
    flex-wrap: wrap;
    gap: 12px;
    font-size: 11px;
    color: var(--text-secondary);
  }

  .subsection {
    margin-top: var(--space-3);
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .subsection-title {
    margin: 0;
    font-size: 12px;
    font-weight: 600;
    color: var(--text-secondary);
  }

  .generation-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .generation-row {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    font-size: 12px;
  }

  .gen-model {
    color: var(--text-primary);
  }

  .gen-coverage {
    color: var(--text-muted);
    font-size: 11px;
  }
</style>
