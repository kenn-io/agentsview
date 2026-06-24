<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import { m } from "../../i18n/index.js";
  import {
    CalendarIcon,
    ChevronDownIcon,
    ChevronLeftIcon,
    ChevronRightIcon,
  } from "../../icons.js";
  import {
    CALENDAR_UNITS,
    RELATIVE_PRESETS,
    calendarLabel,
    rangeLabel,
    resolveRange,
    stepAnchor,
    todayStr,
    type CalendarUnit,
    type RangeMode,
    type RangeSelection,
  } from "./rangeSelection.js";

  interface Props {
    selection: RangeSelection;
    onSelect: (selection: RangeSelection) => void;
    busy?: boolean;
    earliestSession?: string | null;
    /** Popover edge alignment. Defaults to left. */
    align?: "left" | "right";
    /** Future periods to disable Next stepping past (YYYY-MM-DD). */
    maxDate?: string | null;
    /** Stretch the trigger to fill its container (for vertical sidebars). */
    block?: boolean;
  }

  let {
    selection,
    onSelect,
    busy = false,
    earliestSession = null,
    align = "left",
    maxDate = null,
    block = false,
  }: Props = $props();

  const TABS: { mode: RangeMode; label: string }[] = [
    { mode: "relative", label: "Relative" },
    { mode: "calendar", label: "Calendar" },
    { mode: "custom", label: "Custom" },
  ];

  let open = $state(false);
  let containerEl: HTMLDivElement | undefined = $state();

  // Working state for the panel, seeded from `selection` by seed() each time it
  // opens so switching tabs never loses the current range. Edits emit
  // immediately, so these stay consistent with the committed selection while
  // open. `tab` starts on a constant; seed() sets the real tab before the panel
  // ever renders.
  let tab = $state<RangeMode>("relative");
  let calUnit = $state<CalendarUnit>("week");
  let calAnchor = $state<string>(todayStr());
  let customFrom = $state<string>("");
  let customTo = $state<string>("");

  const label = $derived(rangeLabel(selection));
  const stepLabel = $derived(calendarLabel(calUnit, calAnchor));
  // Mirror the activity navigator's conservative guard: disable Next once the
  // anchor is at or past the max date. Only views that pass maxDate are guarded.
  const nextDisabled = $derived(maxDate != null && calAnchor >= maxDate);

  function seed() {
    tab = selection.mode;
    if (selection.mode === "calendar") {
      calUnit = selection.unit;
      calAnchor = selection.anchor;
    } else {
      calUnit = "week";
      calAnchor =
        selection.mode === "custom" && selection.to
          ? selection.to
          : todayStr();
    }
    const resolved = resolveRange(selection, earliestSession);
    customFrom = selection.mode === "custom" ? selection.from : resolved.from;
    customTo = selection.mode === "custom" ? selection.to : resolved.to;
  }

  function toggleOpen() {
    open = !open;
    if (open) seed();
  }

  function isRelativeActive(days: number): boolean {
    return selection.mode === "relative" && selection.days === days;
  }

  // Keep the Custom tab's inputs in step with the latest committed selection so
  // switching tabs after picking a preset edits that range, not a stale seed.
  function syncCustomFields(sel: RangeSelection) {
    const resolved = resolveRange(sel, earliestSession);
    customFrom = resolved.from;
    customTo = resolved.to;
  }

  function applyRelative(days: number) {
    const sel: RangeSelection = { mode: "relative", days };
    calAnchor = resolveRange(sel, earliestSession).to;
    syncCustomFields(sel);
    onSelect(sel);
  }

  function applyCalendar(unit: CalendarUnit, anchor: string) {
    calUnit = unit;
    calAnchor = anchor;
    const sel: RangeSelection = { mode: "calendar", unit, anchor };
    syncCustomFields(sel);
    onSelect(sel);
  }

  function step(dir: -1 | 1) {
    applyCalendar(calUnit, stepAnchor(calUnit, calAnchor, dir));
  }

  function commitCustom() {
    if (!customFrom || !customTo) return;
    // Normalize a reversed range so consumers (and backend validation that
    // rejects to < from) always get ordered bounds; reflect it in the inputs.
    if (customFrom > customTo) {
      const earlier = customTo;
      customTo = customFrom;
      customFrom = earlier;
    }
    onSelect({ mode: "custom", from: customFrom, to: customTo });
  }

  function handleClickOutside(e: MouseEvent) {
    if (containerEl && !containerEl.contains(e.target as Node)) {
      open = false;
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === "Escape" && open) open = false;
  }

  onMount(() => {
    document.addEventListener("click", handleClickOutside);
    document.addEventListener("keydown", handleKeydown);
  });
  onDestroy(() => {
    document.removeEventListener("click", handleClickOutside);
    document.removeEventListener("keydown", handleKeydown);
  });
</script>

<div class="range-picker" bind:this={containerEl} class:busy class:block aria-busy={busy}>
  <button
    class="trigger"
    class:open
    onclick={toggleOpen}
    aria-haspopup="dialog"
    aria-expanded={open}
  >
    <CalendarIcon class="trigger-cal" size="13" strokeWidth="2" aria-hidden="true" />
    <span class="trigger-label">{label}</span>
    <ChevronDownIcon
      class={open ? "trigger-chev open" : "trigger-chev"}
      size="11"
      strokeWidth="2.2"
      aria-hidden="true"
    />
  </button>

  {#if open}
    <div class="panel" class:align-right={align === "right"} role="dialog" aria-label={m.shared_range_select_date_range()}>
      <div class="tabs" role="tablist">
        {#each TABS as t (t.mode)}
          <button
            class="tab"
            class:active={tab === t.mode}
            role="tab"
            aria-selected={tab === t.mode}
            onclick={() => (tab = t.mode)}
          >
            {t.label}
          </button>
        {/each}
      </div>

      {#if tab === "relative"}
        <div class="pills" role="group" aria-label={m.shared_range_relative_window()}>
          {#each RELATIVE_PRESETS as preset (preset.days)}
            <button
              class="pill"
              class:active={isRelativeActive(preset.days)}
              onclick={() => applyRelative(preset.days)}
            >
              {preset.label}
            </button>
          {/each}
        </div>
      {:else if tab === "calendar"}
        <div class="pills" role="group" aria-label={m.shared_range_calendar_period()}>
          {#each CALENDAR_UNITS as u (u.unit)}
            <button
              class="pill"
              class:active={calUnit === u.unit}
              onclick={() => applyCalendar(u.unit, calAnchor)}
            >
              {u.label}
            </button>
          {/each}
        </div>
        <div class="stepper">
          <button
            class="arrow"
            onclick={() => step(-1)}
            aria-label={m.shared_range_previous_period()}
          >
            <ChevronLeftIcon size="15" strokeWidth="2" aria-hidden="true" />
          </button>
          <span class="step-label">{stepLabel}</span>
          <button
            class="arrow"
            onclick={() => step(1)}
            disabled={nextDisabled}
            aria-label={m.shared_range_next_period()}
          >
            <ChevronRightIcon size="15" strokeWidth="2" aria-hidden="true" />
          </button>
        </div>
      {:else}
        <div class="fields">
          <label class="field">
            <span>{m.shared_range_from()}</span>
            <input
              type="date"
              class="date-input"
              bind:value={customFrom}
              onchange={commitCustom}
            />
          </label>
          <label class="field">
            <span>{m.shared_range_to()}</span>
            <input
              type="date"
              class="date-input"
              bind:value={customTo}
              onchange={commitCustom}
            />
          </label>
        </div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .range-picker {
    position: relative;
    display: inline-flex;
  }

  .range-picker.block {
    display: flex;
    width: 100%;
  }

  .range-picker.block .trigger {
    width: 100%;
    justify-content: space-between;
  }

  .range-picker.block .panel {
    width: 100%;
    min-width: 240px;
  }

  .trigger {
    height: 28px;
    /* Hold a stable width so the label changing (e.g. "Jun 19" vs
       "Mar 26 - Apr 25") never resizes the button and shifts neighbors. */
    min-width: 168px;
    padding: 0 10px;
    display: inline-flex;
    align-items: center;
    gap: 7px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-primary);
    font-size: 12px;
    cursor: pointer;
    white-space: nowrap;
    transition: border-color 0.12s, background 0.12s;
  }

  .trigger:hover {
    background: var(--bg-surface-hover);
  }

  .trigger.open {
    border-color: var(--accent-blue);
  }

  .busy .trigger {
    opacity: 0.7;
  }

  :global(.trigger-cal) {
    color: var(--text-muted);
    flex-shrink: 0;
  }

  .trigger-label {
    flex: 1;
    text-align: left;
    font-variant-numeric: tabular-nums;
  }

  :global(.trigger-chev) {
    color: var(--text-muted);
    flex-shrink: 0;
    transition: transform 0.15s;
  }

  :global(.trigger-chev.open) {
    transform: rotate(180deg);
  }

  .panel {
    position: absolute;
    top: calc(100% + 6px);
    left: 0;
    z-index: 100;
    width: 264px;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-lg);
    box-shadow: var(--shadow-lg);
    padding: 10px;
  }

  .panel.align-right {
    left: auto;
    right: 0;
  }

  .tabs {
    display: flex;
    gap: 3px;
    padding: 3px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    margin-bottom: 10px;
  }

  .tab {
    flex: 1;
    height: 26px;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-muted);
    font-size: 12px;
    font-weight: 600;
    cursor: pointer;
    transition: background 0.1s, color 0.1s;
  }

  .tab:hover {
    color: var(--text-secondary);
  }

  .tab.active {
    background: var(--bg-surface);
    color: var(--text-primary);
    box-shadow: var(--shadow-sm);
  }

  .pills {
    display: flex;
    gap: 4px;
  }

  .pill {
    flex: 1;
    height: 30px;
    border: 0;
    border-radius: var(--radius-md);
    background: var(--bg-inset);
    color: var(--text-secondary);
    font-size: 12px;
    font-weight: 500;
    cursor: pointer;
    transition: background 0.1s, color 0.1s;
  }

  .pill:hover {
    background: var(--bg-surface-hover);
  }

  .pill.active {
    background: var(--accent-blue);
    color: #fff;
  }

  .stepper {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-top: 9px;
  }

  .arrow {
    width: 30px;
    height: 30px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
    color: var(--text-secondary);
    cursor: pointer;
    transition: background 0.1s, color 0.1s, opacity 0.1s;
  }

  .arrow:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .arrow:disabled {
    opacity: 0.4;
    cursor: default;
  }

  .step-label {
    flex: 1;
    text-align: center;
    font-size: 12px;
    color: var(--text-secondary);
    font-variant-numeric: tabular-nums;
  }

  .fields {
    display: flex;
    flex-direction: column;
    gap: 7px;
  }

  .field {
    display: flex;
    align-items: center;
    gap: 10px;
  }

  .field > span {
    width: 36px;
    font-size: 11px;
    font-weight: 600;
    color: var(--text-muted);
  }

  .date-input {
    flex: 1;
    height: 30px;
    padding: 0 9px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    color: var(--text-secondary);
    font-family: var(--font-mono);
    font-size: 12px;
    transition: border-color 0.12s;
  }

  .date-input:focus {
    outline: none;
    border-color: var(--accent-blue);
  }
</style>
