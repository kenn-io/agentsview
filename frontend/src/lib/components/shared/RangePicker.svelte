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

  /* Two tiers of one choice. The Relative / Calendar / Custom track is the
     primary control; the presets for the chosen mode hang under it as a
     lower tier. The preset row starts flush against the track's bottom
     edge with a straight, square top, inset so the track overhangs it on
     both sides. Its fill is quieter than the track and only its sides and
     bottom carry a hairline, so it reads as tucked beneath the track rather
     than as a second equal bar. Radii, flat options, and the shared
     selected pill match the segmented controls (app.css). */
  .app-range-picker :global(.kit-date-range-picker__tabs:has(+ .kit-date-range-picker__pills)) {
    margin-bottom: 0;
  }

  .app-range-picker :global(.kit-date-range-picker__pills) {
    --app-presets-radius: var(--kit-control-radius, var(--radius-sm));
    gap: 1px;
    margin: 0 var(--space-6);
    padding: 2px;
    border: 1px solid var(--border-muted);
    border-top: 0;
    border-radius: 0 0 var(--app-presets-radius) var(--app-presets-radius);
    background: color-mix(in srgb, var(--bg-inset) 50%, var(--popover-bg, var(--bg-surface)));
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
