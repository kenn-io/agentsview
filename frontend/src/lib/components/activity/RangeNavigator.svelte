<script lang="ts">
  import { activity } from "../../stores/activity.svelte.js";
  import { ChevronLeftIcon, ChevronRightIcon } from "../../icons.js";

  // Local YYYY-MM-DD for the real clock, used only for the future-period guard.
  function todayStr(): string {
    const now = new Date();
    const y = now.getFullYear();
    const m = String(now.getMonth() + 1).padStart(2, "0");
    const d = String(now.getDate()).padStart(2, "0");
    return `${y}-${m}-${d}`;
  }

  // Conservative date-based future check, carried over from the old day
  // stepper: disable Next once the anchor is at or past today. Precise
  // week/month period-end gating is intentionally deferred.
  const atFuture = $derived(activity.date >= todayStr());

  // Human label for the current non-custom range. Day shows the date alone;
  // week/month prefix the preset so the anchor reads as a period start.
  const rangeLabel = $derived.by(() => {
    if (activity.preset === "week") return `Week of ${activity.date}`;
    if (activity.preset === "month") return `Month of ${activity.date}`;
    return activity.date;
  });

  function stepBack() {
    activity.step(-1);
    activity.load();
  }

  function stepForward() {
    activity.step(1);
    activity.load();
  }

  function onFromInput(e: Event) {
    activity.setFrom((e.currentTarget as HTMLInputElement).value);
    activity.load();
  }

  function onToInput(e: Event) {
    activity.setTo((e.currentTarget as HTMLInputElement).value);
    activity.load();
  }
</script>

{#if activity.preset === "custom"}
  <div class="range-navigator custom">
    <input
      class="date-input"
      type="date"
      value={activity.from}
      oninput={onFromInput}
      aria-label="Range start"
    />
    <span class="date-sep">-</span>
    <input
      class="date-input"
      type="date"
      value={activity.to}
      oninput={onToInput}
      aria-label="Range end"
    />
  </div>
{:else}
  <div class="range-navigator">
    <button
      class="step-btn"
      onclick={stepBack}
      title="Previous"
      aria-label="Previous"
    >
      <ChevronLeftIcon size="14" strokeWidth="2" aria-hidden="true" />
    </button>
    <span class="range-label">{rangeLabel}</span>
    <button
      class="step-btn"
      onclick={stepForward}
      disabled={atFuture}
      title="Next"
      aria-label="Next"
    >
      <ChevronRightIcon size="14" strokeWidth="2" aria-hidden="true" />
    </button>
  </div>
{/if}

<style>
  .range-navigator {
    display: flex;
    align-items: center;
    gap: 4px;
  }

  .step-btn {
    width: 26px;
    height: 26px;
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    cursor: pointer;
    transition: background 0.1s, color 0.1s, opacity 0.1s;
  }

  .step-btn:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .step-btn:disabled {
    cursor: default;
    opacity: 0.4;
  }

  .range-label {
    min-width: 96px;
    text-align: center;
    font-size: 11px;
    color: var(--text-secondary);
    font-variant-numeric: tabular-nums;
  }

  .date-input {
    height: 26px;
    padding: 0 8px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: var(--text-secondary);
    cursor: pointer;
    font-family: var(--font-mono);
  }

  .date-input:focus {
    outline: none;
    border-color: var(--accent-blue);
  }

  .date-sep {
    color: var(--text-muted);
    font-size: 11px;
  }
</style>
