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

  /* The presets are a second segmented control under the Relative /
     Calendar / Custom one, built like kit-ui's: the same inset track,
     radius, flat options, and shared selected pill (app.css). They sit
     below it as their own row, a little shorter and in regular weight, so
     the mode switch stays the first choice. */
  .app-range-picker :global(.kit-date-range-picker__pills) {
    --app-presets-radius: var(--kit-control-radius, var(--radius-sm));
    gap: 1px;
    padding: 2px;
    border-radius: var(--app-presets-radius);
    background: var(--bg-inset);
    box-shadow: var(--shadow-sunken, none);
  }

  .app-range-picker :global(.kit-date-range-picker__pills .kit-date-range-picker__pill) {
    height: 20px;
    border-radius: max(0px, calc(var(--app-presets-radius) - 1px));
    background: transparent;
    color: var(--text-secondary);
    font-size: var(--kit-control-font-size, var(--font-size-xs));
    font-weight: 400;
  }

  .app-range-picker :global(.kit-date-range-picker__pills .kit-date-range-picker__pill:not(.active):hover),
  :global(:root.dark) .app-range-picker :global(.kit-date-range-picker__pills .kit-date-range-picker__pill:not(.active):hover) {
    background: transparent;
    color: var(--text-primary);
  }

  /* Dark secondary ink reads as selected; unselected options step down to
     muted, as in the segmented controls. */
  :global(:root.dark) .app-range-picker :global(.kit-date-range-picker__pills .kit-date-range-picker__pill:not(.active)) {
    color: var(--text-muted);
  }

  .app-range-picker :global(.kit-date-range-picker__pills .kit-date-range-picker__pill.active) {
    background: var(--selected-strong-bg);
    box-shadow: var(--selected-strong-edge);
    color: var(--text-primary);
    font-weight: var(--selected-strong-weight);
  }
</style>
