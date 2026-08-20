# LayerChart migration design

## Goal

Replace the frontend's eight hand-written SVG chart renderers with LayerChart
2.2.0 while preserving their current data, appearance, interaction, and
accessibility contracts.

## Scope

The migration covers these components:

- `activity/ConcurrencyTimeline.svelte`
- `analytics/ActivityTimeline.svelte`
- `analytics/Heatmap.svelte`
- `analytics/HourOfWeekHeatmap.svelte`
- `analytics/SkillTrend.svelte`
- `trends/TrendsLineChart.svelte`
- `usage/CostTimeSeriesChart.svelte`
- `usage/Treemap.svelte`

Compact charts built from semantic HTML and CSS remain unchanged. This includes
bar lists and small distributions in components such as `SessionShape.svelte`,
`ToolUsage.svelte`, and `CacheEfficiencyPanel.svelte`. They do not contain the
manual SVG layout code this migration is intended to remove.

## Dependency and rendering policy

- Add `layerchart` at the exact version `2.2.0`.
- Use LayerChart's Svelte 5 components directly. Do not add a second chart
  wrapper library.
- Prefer LayerChart's SVG layer for the current data sizes and for continuity
  with the existing rendered output.
- Import only the LayerChart and D3 modules required by each chart.
- Keep chart-specific data preparation in the owning component unless two or
  more charts have an identical, stable need.
- Keep the existing chart palette utilities as the source of series identity and
  color. LayerChart must not reassign colors when data is filtered or a series
  is hidden.

## Component design

### Standard Cartesian charts

`ActivityTimeline`, `TrendsLineChart`, and `CostTimeSeriesChart` will use
LayerChart's chart, axis, bar, line, area, grid, and legend building blocks.
LayerChart will own container measurement, scales, tick placement, and SVG path
generation. The components will continue to own data ranking, series folding,
date formatting, metric selection, and application callbacks.

The cost and token time series will retain the existing top-series limit and
`Other` aggregation. A one-point time series must remain visible rather than
collapsing to a zero-width area.

### Heatmaps

`Heatmap` will use LayerChart's calendar composition. `HourOfWeekHeatmap` will
use a rectangular chart with categorical hour and weekday scales. Both will
retain their current level calculation, theme-aware colors, localized labels,
hover readouts, and click or keyboard filtering.

The Sunday-first presentation in the hour-of-week view must not change the
backend's Monday-zero filter values.

### Skill trend

`SkillTrend` will use LayerChart line marks, axes, highlighting, and tooltip
composition. The application will continue to own:

- top-six ranking and `Other` aggregation;
- stable palette assignment across legend changes;
- the interactive legend's pressed state;
- active-locale date formatting;
- the screen-reader data table; and
- keyboard movement between time buckets.

LayerChart's pointer state and the existing keyboard state will feed one shared
selected-bucket readout so pointer and keyboard users receive the same values.

### Treemap

`Treemap` will use LayerChart's treemap layout and marks. It will retain the
current label truncation, cost and token formatting, tile selection callback,
and keyboard activation. Selection continues to mean hiding or exposing the
selected attribution item as defined by the parent component.

### Concurrency timeline

`ConcurrencyTimeline` will use a LayerChart compound chart with separate scales
for concurrency and the optional overlay metric. It will retain:

- stacked interactive and automated concurrency bars;
- the right-side overlay axis and line;
- active and idle strip cells;
- partial-range future shading;
- real bucket-bound widths;
- minute, hour, day, and week labels;
- daylight-saving-time-safe tooltip ranges; and
- pointer and keyboard bucket selection.

LayerChart will own scale ranges and mark geometry. Application code will still
prepare bucket intervals and label text because those are domain rules rather
than renderer concerns.

## Styling and themes

This is a renderer migration, not a dashboard redesign. Chart dimensions,
density, typography, grid treatment, series colors, focus treatment, and card
layout should remain visually consistent with the current pages.

Chart styling will use existing application tokens. LayerChart's optional
framework presets will not be adopted because agentsview already has its own
light and dark themes. Any LayerChart tooltip styling must be scoped under an
agentsview chart container and must use existing surface, border, text, radius,
and shadow tokens.

## Localization

All visible dates, times, numbers, costs, and translated labels must continue to
use the app's active Paraglide locale. Library defaults based only on the
browser locale are not sufficient. No new user-facing copy is planned.

## Accessibility

Charts must keep an accessible name and exact-value alternative. Existing
interactive behavior must remain reachable by keyboard. A pointer-only
LayerChart tooltip is not considered parity.

Application-owned semantic tables, live readouts, button states, and keyboard
handlers remain when LayerChart does not provide an equivalent contract. Tests
will check observable keyboard selection and readout behavior rather than
generated SVG structure.

## Testing

Existing behavior tests remain the contract. Tests that currently assert
hand-written path strings, SVG element counts, or renderer-specific classes will
be replaced only when those assertions describe implementation rather than user
behavior.

Focused tests will cover:

- palette stability when series are hidden or filtered;
- localized date and currency labels;
- top-series and `Other` aggregation;
- pointer and keyboard selection;
- backend filter values emitted by heatmaps and timeline buckets;
- one-point and empty datasets; and
- the concurrency overlay and future region.

The completed migration must pass the frontend unit suite, Svelte type checking,
kit-ui validation, and a production build. The Analytics, Usage, Trends, and
Activity pages will also be inspected in a browser in light and dark modes at
desktop and narrow widths.

## Delivery

The work will proceed by chart family so each commit leaves a working frontend:

1. Add LayerChart and migrate shared Cartesian behavior.
1. Migrate the heatmaps and treemap.
1. Migrate SkillTrend and its interaction contract.
1. Migrate ConcurrencyTimeline and its compound axes.
1. Run full verification and visual comparison.

No compatibility switch or dual-rendering path will ship. Git history provides
the rollback path during review.

## Non-goals

- Redesigning chart cards, controls, or surrounding pages.
- Changing backend aggregation or API contracts.
- Converting semantic HTML/CSS microcharts.
- Adding animation, zooming, brushing, or export features that do not exist
  today.
- Replacing the existing application palette or localization system.
