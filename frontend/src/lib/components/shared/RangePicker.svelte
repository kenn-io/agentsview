<script lang="ts">
  import { RangePicker as KitRangePicker, type RangePreset } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
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

  // kit-ui appends the week-start date after this label, so only the static
  // text of the app template survives. Date-first locales (zh) lose their
  // word order here until kit-ui grows a date-position slot.
  const weekOfLabel = $derived(m.shared_range_week_of({ date: "" }).trim());
</script>

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
