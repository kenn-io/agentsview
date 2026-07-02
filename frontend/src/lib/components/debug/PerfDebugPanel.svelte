<script lang="ts">
  import { m } from "../../i18n/index.js";
  import { IconButton } from "@kenn-io/kit-ui";
  import { perf } from "../../stores/perf.svelte.js";
  import { XIcon } from "../../icons.js";

  const entries = $derived(perf.entries.slice(0, 80));
  const slowest = $derived(
    [...perf.entries].sort((a, b) => b.durationMs - a.durationMs).slice(0, 3),
  );

  function formatMs(value: number): string {
    return value >= 1000
      ? `${(value / 1000).toFixed(2)}s`
      : `${Math.round(value)}ms`;
  }

  function formatBytes(value: number | undefined): string {
    if (value === undefined) return "";
    if (value >= 1024 * 1024) {
      return `${(value / (1024 * 1024)).toFixed(1)} MB`;
    }
    if (value >= 1024) return `${Math.round(value / 1024)} KB`;
    return `${value} B`;
  }

  function formatTime(value: number): string {
    return new Date(value).toLocaleTimeString();
  }
</script>

{#if perf.panelOpen}
  <section
    class="perf-panel kit-popover-card"
    aria-label={m.perf_aria_label()}
  >
    <header class="perf-header">
      <div>
        <div class="title">{m.perf_title()}</div>
        <div class="subtitle">{m.perf_samples({ count: perf.entries.length })}</div>
      </div>
      <div class="header-actions">
        <button
          class="text-btn"
          onclick={() => perf.clear()}
          disabled={perf.entries.length === 0}
        >
          {m.perf_clear()}
        </button>
        <IconButton
          size="sm"
          onclick={() => (perf.panelOpen = false)}
          title={m.perf_close()}
          ariaLabel={m.perf_close_aria()}
        >
          <XIcon size="14" strokeWidth="2" aria-hidden="true" />
        </IconButton>
      </div>
    </header>

    {#if slowest.length > 0}
      <div class="slow-strip">
        {#each slowest as entry (entry.id)}
          <div class="slow-item">
            <span class="slow-name">{entry.route}.{entry.name}</span>
            <span>{formatMs(entry.durationMs)}</span>
          </div>
        {/each}
      </div>
    {/if}

    <div class="entry-table" role="table">
      <div class="entry-row entry-heading" role="row">
        <span>{m.perf_col_time()}</span>
        <span>{m.perf_col_route()}</span>
        <span>{m.perf_col_name()}</span>
        <span>{m.perf_col_duration()}</span>
        <span>{m.perf_col_status()}</span>
      </div>
      {#each entries as entry (entry.id)}
        <div class="entry-row" role="row">
          <span>{formatTime(entry.at)}</span>
          <span>{entry.route}</span>
          <span class="entry-name" title={entry.path ?? entry.name}>
            {entry.kind === "api" && entry.method ? `${entry.method} ` : ""}
            {entry.name}
            {#if entry.sizeBytes !== undefined}
              <span class="entry-size">{formatBytes(entry.sizeBytes)}</span>
            {/if}
          </span>
          <span class:slow={entry.durationMs >= 1000}>
            {formatMs(entry.durationMs)}
          </span>
          <span>{entry.status}</span>
        </div>
      {/each}
    </div>
  </section>
{/if}

<style>
  .perf-panel {
    position: fixed;
    right: 12px;
    bottom: calc(var(--status-bar-height, 24px) + 12px);
    z-index: 10020;
    width: min(720px, calc(100vw - 24px));
    max-height: min(520px, calc(100vh - 96px));
    display: flex;
    flex-direction: column;
    /* card chrome comes from the shared kit-popover-card class */
    color: var(--text-primary);
    overflow: hidden;
  }

  .perf-header {
    min-height: 42px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    padding: 8px 10px 8px 12px;
    border-bottom: 1px solid var(--border-muted);
  }

  .title {
    font-size: 12px;
    font-weight: 600;
  }

  .subtitle {
    margin-top: 1px;
    font-size: 10px;
    color: var(--text-muted);
  }

  .header-actions {
    display: flex;
    align-items: center;
    gap: 4px;
  }

  .text-btn {
    height: 24px;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    padding: 0 8px;
    font-size: 11px;
  }

  .text-btn:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .text-btn:disabled {
    opacity: 0.5;
  }

  .slow-strip {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 1px;
    background: var(--border-muted);
    border-bottom: 1px solid var(--border-muted);
  }

  .slow-item {
    min-width: 0;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 7px 10px;
    background: var(--bg-inset);
    font-size: 11px;
    color: var(--text-secondary);
  }

  .slow-name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .entry-table {
    overflow: auto;
    font-size: 11px;
  }

  .entry-row {
    display: grid;
    grid-template-columns: 74px 76px minmax(180px, 1fr) 72px 64px;
    gap: 8px;
    align-items: center;
    min-height: 28px;
    padding: 0 10px;
    border-bottom: 1px solid var(--border-subtle, var(--border-muted));
  }

  .entry-heading {
    position: sticky;
    top: 0;
    z-index: 1;
    min-height: 26px;
    background: var(--bg-surface);
    color: var(--text-muted);
    font-size: 10px;
    font-weight: 600;
  }

  .entry-name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-family: var(--font-mono);
  }

  .entry-size {
    margin-left: 6px;
    color: var(--text-muted);
    font-family: var(--font-sans);
  }

  .slow {
    color: var(--accent-red);
    font-weight: 600;
  }

  @media (max-width: 640px) {
    .slow-strip {
      grid-template-columns: 1fr;
    }

    .entry-row {
      grid-template-columns: 68px 1fr 64px;
    }

    .entry-row span:nth-child(2),
    .entry-row span:nth-child(5) {
      display: none;
    }
  }
</style>
