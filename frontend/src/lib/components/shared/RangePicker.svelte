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
     both sides. Its top row is one contact line across its full width,
     drawn over a quiet hairline ring so the top corners stay square, which
     shows the track resting on the tier. Its fill, height, and type sit a
     step below the track. Radii, flat options, and the shared selected
     pill match the segmented controls (app.css). */
  .app-range-picker :global(.kit-date-range-picker__tabs:has(+ .kit-date-range-picker__pills)) {
    margin-bottom: 0;
  }

  .app-range-picker :global(.kit-date-range-picker__pills) {
    --app-presets-radius: var(--kit-control-radius, var(--radius-sm));
    gap: 1px;
    margin: 0 calc(var(--space-6) + 4px);
    /* One pixel more above and below than the track, for the contact line
       and so tier fill shows around the chosen preset. */
    padding: 3px;
    border: 0;
    border-radius: 0 0 var(--app-presets-radius) var(--app-presets-radius);
    background: var(--app-presets-fill);
    /* The first shadow paints on top: the contact line covers the ring's
       top row, corners included. */
    box-shadow:
      inset 0 1px 0 var(--app-presets-contact),
      inset 0 0 0 1px var(--app-presets-edge);
    --app-presets-fill: color-mix(in srgb, var(--bg-inset) 68%, var(--popover-bg, var(--bg-surface)));
    --app-presets-edge: color-mix(in srgb, var(--border-default) 50%, var(--border-muted));
    --app-presets-contact: color-mix(in srgb, var(--border-default) 75%, var(--border-muted));
  }

  /* Dark: the tier sits a step above the near-black track with a quiet
     outline, and a near-black contact line shows the track resting on it. */
  :global(:root.dark) .app-range-picker :global(.kit-date-range-picker__pills) {
    --app-presets-fill: color-mix(in srgb, var(--bg-inset) 35%, var(--popover-bg, var(--bg-surface)));
    --app-presets-edge: color-mix(in srgb, var(--text-primary) 17%, var(--bg-surface));
    --app-presets-contact: rgba(0, 0, 0, 0.55); /* kit-ui-check-ignore: dark tier contact line, upstream later */
  }

  .app-range-picker :global(.kit-date-range-picker__pills .kit-date-range-picker__pill) {
    height: 17px;
    border-radius: max(0px, calc(var(--app-presets-radius) - 1px));
    background: transparent;
    color: var(--text-secondary);
    /* A step below the mode labels, so the shared selected pill on a
       preset still reads as the lesser choice. */
    font-size: calc(var(--kit-control-font-size, var(--font-size-xs)) - 1px);
    font-weight: 400;
  }

  .app-range-picker :global(.kit-date-range-picker__pills .kit-date-range-picker__pill:not(.active):hover),
  :global(:root.dark) .app-range-picker :global(.kit-date-range-picker__pills .kit-date-range-picker__pill:not(.active):hover) {
    background: transparent;
    color: var(--text-primary);
  }

  /* Dark secondary ink reads as selected; unselected options step down to
     muted, the same ink as the unselected mode tabs. */
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
