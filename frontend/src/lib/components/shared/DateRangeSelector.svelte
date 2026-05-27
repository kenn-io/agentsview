<script lang="ts">
  import { sync } from "../../stores/sync.svelte.js";
  import {
    activePresetDays,
    DATE_RANGE_PRESETS,
    presetRange,
  } from "./dateRangeSelector.js";

  interface Props {
    from: string;
    to: string;
    onChange: (from: string, to: string) => void;
    onPreset?: (days: number) => void;
    rollingDays?: number | null;
    isPinned?: boolean;
  }

  let {
    from,
    to,
    onChange,
    onPreset,
    rollingDays = null,
    isPinned = true,
  }: Props = $props();

  const earliestSession = $derived(sync.stats?.earliest_session ?? null);
  const activeDays = $derived(
    activePresetDays(
      from,
      to,
      earliestSession,
      rollingDays,
      isPinned,
    ),
  );

  function applyPreset(days: number) {
    if (days > 0 && onPreset) {
      onPreset(days);
      return;
    }
    const range = presetRange(days, earliestSession);
    onChange(range.from, range.to);
  }

  function handleFromChange(
    e: Event & { currentTarget: HTMLInputElement },
  ) {
    const val = e.currentTarget.value;
    if (val) onChange(val, to);
  }

  function handleToChange(
    e: Event & { currentTarget: HTMLInputElement },
  ) {
    const val = e.currentTarget.value;
    if (val) onChange(from, val);
  }
</script>

<div class="date-range-picker">
  <div class="presets">
    {#each DATE_RANGE_PRESETS as preset}
      <button
        class="preset-btn"
        class:active={activeDays === preset.days}
        onclick={() => applyPreset(preset.days)}
      >
        {preset.label}
      </button>
    {/each}
  </div>

  <div class="date-inputs">
    <input
      type="date"
      class="date-input"
      value={from}
      onchange={handleFromChange}
    />
    <span class="date-sep">-</span>
    <input
      type="date"
      class="date-input"
      value={to}
      onchange={handleToChange}
    />
  </div>
</div>

<style>
  .date-range-picker {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 8px 12px;
    min-width: 0;
  }

  .presets {
    display: flex;
    flex: 0 0 auto;
    gap: 2px;
  }

  .preset-btn {
    height: 24px;
    padding: 0 8px;
    border: 1px solid transparent;
    border-radius: var(--radius-sm);
    background: transparent;
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    cursor: pointer;
    transition: background 0.1s, color 0.1s;
  }

  .preset-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .preset-btn.active {
    background: var(--accent-blue);
    border-color: var(--accent-blue);
    color: var(--text-on-accent, oklch(0.99 0.006 260));
  }

  .date-inputs {
    display: flex;
    align-items: center;
    flex: 0 1 auto;
    gap: 4px;
    min-width: 0;
  }

  .date-input {
    height: 24px;
    padding: 0 6px;
    background: var(--bg-inset);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: var(--text-secondary);
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

  @media (max-width: 680px) {
    .date-range-picker {
      align-items: stretch;
      flex-direction: column;
      gap: 6px;
      width: 100%;
    }

    .presets {
      flex-wrap: wrap;
    }

    .date-inputs {
      display: grid;
      grid-template-columns: minmax(0, 1fr) auto minmax(0, 1fr);
    }

    .date-input {
      min-width: 0;
      width: 100%;
    }
  }
</style>
