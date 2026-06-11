<script lang="ts">
  import { sync } from "../../stores/sync.svelte.js";
  import {
    DATE_RANGE_PRESETS,
    isPresetActive,
    presetRange,
  } from "./dateRangeSelector.js";

  interface Props {
    from: string;
    to: string;
    busy?: boolean;
    onChange: (from: string, to: string) => void;
    onPreset?: (days: number) => void;
  }

  let {
    from,
    to,
    busy = false,
    onChange,
    onPreset,
  }: Props = $props();

  const earliestSession = $derived(sync.stats?.earliest_session ?? null);

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

<div class="date-range-picker" class:busy aria-busy={busy}>
  <div class="presets">
    {#each DATE_RANGE_PRESETS as preset}
      <button
        class="preset-btn"
        class:active={isPresetActive(
          from,
          to,
          preset.days,
          earliestSession,
        )}
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

  {#if busy}
    <span class="range-busy" aria-live="polite">
      <span class="range-spinner" aria-hidden="true"></span>
      Updating
    </span>
  {/if}
</div>

<style>
  .date-range-picker {
    display: flex;
    align-items: center;
    gap: 12px;
  }

  .date-range-picker.busy .preset-btn.active {
    background: color-mix(
      in srgb,
      var(--accent-blue) 72%,
      var(--bg-surface)
    );
  }

  .presets {
    display: flex;
    gap: 2px;
  }

  .preset-btn {
    height: 24px;
    padding: 0 8px;
    border-radius: var(--radius-sm);
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
    color: #fff;
  }

  .date-inputs {
    display: flex;
    align-items: center;
    gap: 4px;
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

  .range-busy {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    color: var(--accent-blue);
    font-size: 11px;
    font-weight: 500;
    white-space: nowrap;
  }

  .range-spinner {
    width: 11px;
    height: 11px;
    border-radius: 50%;
    border: 2px solid color-mix(
      in srgb,
      var(--accent-blue) 28%,
      transparent
    );
    border-top-color: var(--accent-blue);
    animation: spin 0.8s linear infinite;
  }

  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
</style>
