<script lang="ts">
  import { DateRangePicker as KitRangePicker, type RangePreset } from "@kenn-io/kit-ui";
  import { getLocale, m } from "../../i18n/index.js";
  import { RELATIVE_PRESETS, type RangeSelection } from "./rangeSelection.js";

  // Thin wrapper over kit-ui's RangePicker: same call-site API as the old
  // local component, with all user-facing strings injected from the app's
  // localized messages in one place. Range semantics for consumers are
  // unchanged -- pages keep resolving selections through the app's own
  // resolveRange() in rangeSelection.ts.

  interface Props {
    selection: RangeSelection;
    onSelect: (selection: RangeSelection) => void;
    busy?: boolean;
    /** Earliest known session date; anchors the "All" preset. */
    earliestSession?: string | null;
    /** Popover edge alignment. Defaults to left. */
    align?: "left" | "right";
    /** Later dates are disabled in the calendar grid (YYYY-MM-DD). */
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

  // kit-ui presets carry plain strings; render the app's localized presets.
  const presets = $derived<RangePreset[]>(
    RELATIVE_PRESETS.map((p) => ({
      label: p.label(),
      longLabel: p.longLabel(),
      days: p.days,
    })),
  );

  // kit-ui substitutes "{days}" into this template itself, so render the app
  // message with a sentinel count and swap it for kit-ui's placeholder. The
  // plural "other" form is used for every non-preset day count.
  const lastDaysLabel = $derived(
    m.shared_range_last_days({ count: 0 }).replace("0", "{days}"),
  );

  // kit-ui substitutes "{date}" into this template itself, so pass the app
  // message through with its slot intact — date-first locales (zh,
  // "{date}所在周") keep their word order.
  const weekOfLabel = $derived(m.shared_range_week_of({ date: "{date}" }));
</script>

<!-- The wrapper only scopes the panel styles below; it adds no box. -->
<div class="app-range-picker">
<KitRangePicker
  {selection}
  {onSelect}
  {busy}
  earliestDate={earliestSession}
  {align}
  {maxDate}
  {block}
  {presets}
  {lastDaysLabel}
  {weekOfLabel}
  locale={getLocale()}
  relativeTabLabel={m.shared_range_tab_relative()}
  calendarTabLabel={m.shared_range_tab_calendar()}
  customTabLabel={m.shared_range_tab_custom()}
  dayLabel={m.shared_range_calendar_day()}
  weekLabel={m.shared_range_calendar_week()}
  monthLabel={m.shared_range_calendar_month()}
  customRangeLabel={m.shared_range_custom_range()}
  fromLabel={m.shared_range_from()}
  toLabel={m.shared_range_to()}
  dialogLabel={m.shared_range_select_date_range()}
  relativeGroupLabel={m.shared_range_relative_window()}
  calendarGroupLabel={m.shared_range_calendar_period()}
  previousMonthLabel={m.shared_range_previous_period()}
  nextMonthLabel={m.shared_range_next_period()}
  previousYearLabel={m.shared_range_previous_year()}
  nextYearLabel={m.shared_range_next_year()}
  previousYearsLabel={m.shared_range_previous_years()}
  nextYearsLabel={m.shared_range_next_years()}
  chooseMonthLabel={m.shared_range_choose_month()}
  chooseYearLabel={m.shared_range_choose_year()}
/>
</div>

<style>
  .app-range-picker {
    display: contents;
  }

  /* Two levels of one choice: the Relative / Calendar / Custom track on
     top, and the option row for the chosen mode as a narrower tray that
     hangs from it. The tray shares the track's fill and joins its bottom
     edge, inset from both ends, so it reads as coming out of the mode
     control. Its options are smaller and flat; only the chosen mode is a
     raised pill. */
  .app-range-picker :global(.kit-date-range-picker__tabs:has(+ .kit-date-range-picker__pills)) {
    margin-bottom: 0;
  }

  .app-range-picker :global(.kit-date-range-picker__pills) {
    --tray-fillet: 6px;
    position: relative;
    gap: 2px;
    margin: 0 var(--space-5);
    padding: 3px 4px 4px;
    border-radius: 0 0 var(--radius-md) var(--radius-md);
    background: var(--bg-inset);
    box-shadow: var(--shadow-sunken, none);
  }

  /* Concave corners where the tray meets the track, so it flows out of
     it instead of sitting under it. */
  .app-range-picker :global(.kit-date-range-picker__pills::before),
  .app-range-picker :global(.kit-date-range-picker__pills::after) {
    content: "";
    position: absolute;
    top: 0;
    width: var(--tray-fillet);
    height: var(--tray-fillet);
    pointer-events: none;
  }

  .app-range-picker :global(.kit-date-range-picker__pills::before) {
    right: 100%;
    background: radial-gradient(
      circle at 0 100%,
      transparent calc(var(--tray-fillet) - 0.5px),
      var(--bg-inset) var(--tray-fillet)
    );
  }

  .app-range-picker :global(.kit-date-range-picker__pills::after) {
    left: 100%;
    background: radial-gradient(
      circle at 100% 100%,
      transparent calc(var(--tray-fillet) - 0.5px),
      var(--bg-inset) var(--tray-fillet)
    );
  }

  .app-range-picker :global(.kit-date-range-picker__pills .kit-date-range-picker__pill) {
    height: 24px;
    border-radius: var(--radius-sm);
    background: transparent;
    box-shadow: none;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 400;
  }

  .app-range-picker :global(.kit-date-range-picker__pills .kit-date-range-picker__pill:not(.active):hover),
  :global(:root.dark) .app-range-picker :global(.kit-date-range-picker__pills .kit-date-range-picker__pill:not(.active):hover) {
    background: transparent;
    color: var(--text-primary);
  }

  :global(:root.dark) .app-range-picker :global(.kit-date-range-picker__pills .kit-date-range-picker__pill:not(.active)) {
    color: var(--text-muted);
  }

  /* The chosen option is a flat tint with primary ink: clearly picked,
     but quieter than the raised mode pill above it. */
  .app-range-picker :global(.kit-date-range-picker__pills .kit-date-range-picker__pill.active) {
    background: color-mix(in srgb, var(--text-primary) 8%, transparent);
    box-shadow: none;
    color: var(--text-primary);
    font-weight: var(--font-weight-medium, 500);
  }
</style>
