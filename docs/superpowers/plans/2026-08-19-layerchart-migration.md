# LayerChart migration implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace all eight hand-written SVG chart renderers with LayerChart
2.2.0 without changing their data, filtering, visual identity, localization, or
accessibility contracts.

**Architecture:** Each chart keeps its existing data shaping and application
callbacks, then gives prepared rows or series to LayerChart for measurement,
scales, layout, and SVG marks. Application-owned controls, exact-value tables,
keyboard handlers, and stable palette maps remain outside the library. Tests
assert these product contracts through app-owned attributes and accessible DOM,
not LayerChart's internal element structure.

**Tech stack:** Svelte 5.56.7, TypeScript 6.0.3, LayerChart 2.2.0, D3 scale and
hierarchy modules, Vitest through Vite+, Paraglide JS, and Playwright.

**Spec:** `docs/superpowers/specs/2026-08-19-layerchart-migration-design.md`

## Global constraints

- Pin `layerchart` to exactly `2.2.0`.
- Use LayerChart's Svelte 5 components directly. Do not add a chart wrapper.
- Keep the current chart dimensions, palette assignments, controls, callbacks,
  empty states, and error states.
- Format visible dates, numbers, and money with the active Paraglide locale.
- Keep pointer and keyboard access to every current chart action.
- Keep exact-value tables or live readouts where a graphic alone is not enough.
- Do not add animation, zoom, brush, export, feature flags, or dual rendering.
- Do not change backend aggregation, request parameters, or response types.
- Before each commit, use `kenn:commit`. Never bypass hooks.

Every migrated chart gets an app-owned accessible name and exact-value
alternative. Reuse current Paraglide messages for captions and column labels;
this migration adds no catalogue keys. Use `class="kit-sr-only"` tables for
Cartesian charts, heatmaps, SkillTrend, and ConcurrencyTimeline. Use a
`kit-sr-only` list for Treemap. The source rows, not rendered coordinates,
populate these alternatives.

______________________________________________________________________

### Task 1: Migrate the Cartesian charts

**Files:**

- Modify: `frontend/package.json`
- Modify: `frontend/package-lock.json`
- Modify: `frontend/src/lib/components/analytics/ActivityTimeline.svelte`
- Create: `frontend/src/lib/components/analytics/ActivityTimeline.test.ts`
- Modify: `frontend/src/lib/components/trends/TrendsLineChart.svelte`
- Create: `frontend/src/lib/components/trends/TrendsLineChart.test.ts`
- Modify: `frontend/src/lib/components/usage/CostTimeSeriesChart.svelte`
- Modify: `frontend/src/lib/components/usage/CostTimeSeriesChart.test.ts`

**Interfaces:**

- Consumes: the existing analytics and usage stores, `TrendsBucket[]`,
  `TrendsSeries[]`, and caller-supplied palette callbacks.

- Produces: unchanged component props and callbacks; app-owned test hooks
  `data-activity-date`, `data-trend-term`, and `data-time-series-key` on the
  rendered marks.

- [ ] **Step 1: Install the pinned renderer and direct D3 imports**

Run from `frontend/`:

```bash
vp install
vp add -E layerchart@2.2.0 d3-hierarchy@3.1.2 d3-scale@4.0.2
vp add -D -E @types/d3-hierarchy@3.1.7 @types/d3-scale@4.0.9
```

The two D3 packages are direct dependencies because the app imports `scaleBand`,
`scaleLinear`, `scaleOrdinal`, and `hierarchy`. Do not add `d3-shape`;
LayerChart will generate the line and area paths.

- [ ] **Step 2: Record the focused baseline**

Run:

```bash
vp test run src/lib/components/usage/CostTimeSeriesChart.test.ts src/lib/components/analytics/AnalyticsPage.test.ts src/lib/components/trends/TrendsPage.test.ts
```

Expected: PASS before the renderer changes.

- [ ] **Step 3: Add behavior tests for the activity bars**

Create `ActivityTimeline.test.ts` with jsdom setup that resets `analytics` and
the locale after each case. Protect week-range keyboard selection and localized
visible dates with concrete data:

```ts
it("selects a week range from the keyboard", async () => {
  analytics.granularity = "week";
  analytics.activity = {
    granularity: "week",
    series: [{
      date: "2026-08-17",
      sessions: 2,
      messages: 7,
      user_messages: 3,
      assistant_messages: 4,
      tool_calls: 0,
      thinking_messages: 0,
      by_agent: {},
    }],
  };
  const onDateRangeChange = vi.fn();
  const component = mount(ActivityTimeline, {
    target: document.body,
    props: { onDateRangeChange },
  });
  await tick();

  const bar = document.querySelector<SVGElement>(
    '[data-activity-date="2026-08-17"]',
  )!;
  bar.dispatchEvent(new KeyboardEvent("keydown", {
    key: "Enter",
    bubbles: true,
  }));

  expect(onDateRangeChange).toHaveBeenCalledWith(
    "2026-08-17",
    "2026-08-23",
  );
  unmount(component);
});
```

This test fails before the migration because the app-owned date attribute does
not exist.

- [ ] **Step 4: Add behavior tests for trends marks**

Create `TrendsLineChart.test.ts`. Mount two terms in volume order, set one as
active, and assert term identity, palette stability, and the public hover
callback:

```ts
it("keeps term colors stable and reports the hovered term", async () => {
  const onHover = vi.fn();
  const colors = new Map([
    ["alpha", "#1f77b4"],
    ["beta", "#ff7f0e"],
  ]);
  const component = mount(TrendsLineChart, {
    target: document.body,
    props: {
      buckets: [
        { date: "2026-08-10", message_count: 10 },
        { date: "2026-08-17", message_count: 20 },
      ],
      series: [
        {
          term: "beta",
          variants: [],
          total: 8,
          points: [
            { date: "2026-08-10", count: 3 },
            { date: "2026-08-17", count: 5 },
          ],
        },
        {
          term: "alpha",
          variants: [],
          total: 4,
          points: [
            { date: "2026-08-10", count: 2 },
            { date: "2026-08-17", count: 2 },
          ],
        },
      ],
      colorFor: (term: string) => colors.get(term)!,
      activeTerm: null,
      normalized: false,
      onHover,
    },
  });
  await tick();

  const beta = document.querySelector<SVGElement>(
    '[data-trend-term="beta"]',
  )!;
  expect(beta.getAttribute("stroke")).toBe("#ff7f0e");
  beta.dispatchEvent(new PointerEvent("pointerenter", { bubbles: true }));
  expect(onHover).toHaveBeenLastCalledWith("beta");
  beta.dispatchEvent(new PointerEvent("pointerleave", { bubbles: true }));
  expect(onHover).toHaveBeenLastCalledWith(null);
  unmount(component);
});
```

- [ ] **Step 5: Rewrite cost-series assertions around product behavior**

In `CostTimeSeriesChart.test.ts`, replace queries for `path[opacity='0.7']` and
path-string prefixes with `[data-time-series-key]`. Retain the literal color
expectations, distinct project-key expectations, top-five plus `Other` behavior,
selected-token scale, and French currency bounds. Add the single-point contract:

```ts
it("renders a one-day series as a visible mark", async () => {
  usage.toggles.timeSeries.groupBy = "model";
  usage.summary = usageSummary();
  usage.summary.daily = [
    modelDailyEntry(0, [
      { modelName: "single-model", cost: testMoney(6) },
    ]),
  ];

  const component = mountChart();
  await tick();

  expect(document.querySelector(
    '[data-time-series-key="single-model"]',
  )).not.toBeNull();
  unmount(component);
});
```

- [ ] **Step 6: Run the new tests and confirm the red state**

Run:

```bash
vp test run src/lib/components/analytics/ActivityTimeline.test.ts src/lib/components/trends/TrendsLineChart.test.ts src/lib/components/usage/CostTimeSeriesChart.test.ts
```

Expected: FAIL because the three app-owned mark attributes do not exist and the
cost chart still uses its own path builder.

- [ ] **Step 7: Replace ActivityTimeline geometry with LayerChart**

Keep metric selection, ranking, date-range callbacks, error handling, and the
application tooltip. Replace the `ResizeObserver`, pixel bar calculations, and
outer `<svg>` with a band-scale `Chart`, `Axis`, `Grid`, and one `Bar` per row:

```svelte
<Chart
  data={chartData}
  x="date"
  y="value"
  xScale={scaleBand().paddingInner(0.12).paddingOuter(0.02)}
  yDomain={[0, null]}
  yNice
  padding={{ top: 20, right: 4, bottom: 20, left: 0 }}
  height={164}
>
  {#snippet children()}
    <Layer>
      <Grid x={false} y class="grid-line" />
      <Axis
        placement="bottom"
        format={(date) => formatDateLabel(String(date))}
        tickValues={xTickDates}
        rule={false}
      />
      {#each chartData as bar (bar.date)}
        <Bar
          data={bar}
  data-activity-date={bar.date}
          class:empty={bar.value === 0}
          class:selected={analytics.selectedDate === bar.date}
          class:dimmed={analytics.selectedDate !== null && analytics.selectedDate !== bar.date}
          role="button"
          tabindex="0"
          onclick={() => handleBarClick(bar)}
          onkeydown={(event) => handleBarKeydown(event, bar)}
          onpointerenter={(event) => handleBarHover(event, bar)}
          onpointerleave={handleBarLeave}
        />
      {/each}
    </Layer>
  {/snippet}
</Chart>
```

Use `formatDateTime(parseLocalDate(date), options)` and `getLocale()` for every
visible date and number. Keep the sparse tick-date calculation but remove all
pixel `x`, `y`, width, height, and SVG-width calculations.

Add a visually hidden table captioned with `m.analytics_activity_title()`. Each
row contains the localized date, sessions, messages, user messages, and
assistant messages from the original activity entry.

- [ ] **Step 8: Replace TrendsLineChart paths with LayerChart**

Prepare one point array per term with `{ index, date, value }`. Give the flat
point set to `Chart` for the common domains, then render one LayerChart `Line`
per term. Retain the wide transparent hit line, active-term opacity, markers,
axis density, empty overlay, and `onHover` callback:

```svelte
<Chart
  data={flatPoints}
  x="index"
  y="value"
  yDomain={[0, null]}
  yNice
  padding={{ top: 28, right: 12, bottom: 34, left: 52 }}
  height={300}
>
  <Layer>
    <Axis placement="left" grid format={formatMetric} />
    <Axis placement="bottom" tickValues={xTickIndexes} format={formatBucket} />
    {#each plotSeries as item (item.term)}
      <Line
        data={item.points}
        data-trend-term={item.term}
        stroke={colorFor(item.term, item.colorIndex)}
        strokeWidth={activeTerm === item.term ? 3 : 2}
        strokeOpacity={activeTerm !== null && activeTerm !== item.term ? 0.24 : 1}
        onpointerenter={() => onHover(item.term)}
        onpointerleave={() => onHover(null)}
      />
    {/each}
  </Layer>
</Chart>
```

Use `formatDateTime` for x-axis dates and `getLocale()` for metric values. Keep
the existing `role="img"` and localized accessible name on an app-owned chart
container. Add a visually hidden table captioned with the same accessible name.
Rows are buckets, columns are terms, and cells contain the displayed raw or
normalized value.

- [ ] **Step 9: Replace CostTimeSeriesChart path building with LayerChart**

Keep the top-five ranking, `__other__` fold, project ID/label split, selected
token calculation, y-label width calculation, and palette map. Convert each wide
point into stacked rows with `y0` and `y1` values. Use LayerChart `Area` marks
for two or more dates and LayerChart `Bar` marks for one date:

```ts
interface StackRow {
  date: string;
  index: number;
  key: string;
  y0: number;
  y1: number;
}

const stackedSeries = $derived.by(() => {
  const baselines = new Float64Array(seriesData.points.length);
  return seriesData.keys.map((key) => ({
    key,
    color: key === "__other__"
      ? "var(--text-muted)"
      : colorMap.get(key) ?? "var(--text-muted)",
    points: seriesData.points.map((point, index) => {
      const y0 = baselines[index]!;
      const y1 = y0 + (point.values[key] ?? 0);
      baselines[index] = y1;
      return { date: point.date, index, key, y0, y1 };
    }),
  }));
});
```

Then render:

```svelte
{#if seriesData.points.length === 1}
  {#each stackedSeries as item (item.key)}
    <Bar
      data={item.points[0]}
      y={[(row) => row.y0, (row) => row.y1]}
      fill={item.color}
      data-time-series-key={item.key}
    />
  {/each}
{:else}
  {#each stackedSeries as item (item.key)}
    <Area
      data={item.points}
      y={[(row) => row.y0, (row) => row.y1]}
      fill={item.color}
      fillOpacity={0.7}
      data-time-series-key={item.key}
    />
  {/each}
{/if}
```

LayerChart owns both scales and mark geometry. Keep app formatters on `Axis` so
French `$US` labels and the rightmost localized date remain inside the chart
padding. Remove `scaleY`, `buildPaths`, `paths`, manual x-label coordinates, and
the hand-written chart `<svg>`.

Add a visually hidden table captioned with the current cost-over-time or
tokens-over-time title. Rows are dates, columns follow `seriesData.keys`, and
cells contain the formatted unstacked value for that date and key.

- [ ] **Step 10: Run focused checks**

Run:

```bash
vp test run src/lib/components/analytics/ActivityTimeline.test.ts src/lib/components/trends/TrendsLineChart.test.ts src/lib/components/usage/CostTimeSeriesChart.test.ts
vp check
vp run check:kit-ui
```

Expected: PASS.

- [ ] **Step 11: Commit the Cartesian migration**

Use `kenn:commit` to stage only the package files, the three chart components,
and their three test files. Use the subject:

```text
feat(frontend): migrate Cartesian charts to LayerChart
```

### Task 2: Migrate heatmaps and treemap

**Files:**

- Modify: `frontend/src/lib/components/analytics/Heatmap.svelte`
- Create: `frontend/src/lib/components/analytics/Heatmap.test.ts`
- Modify: `frontend/src/lib/components/analytics/HourOfWeekHeatmap.svelte`
- Modify: `frontend/src/lib/components/analytics/HourOfWeekHeatmap.test.ts`
- Modify: `frontend/src/lib/components/usage/Treemap.svelte`
- Create: `frontend/src/lib/components/usage/Treemap.test.ts`
- Delete: `frontend/src/lib/utils/treemap.ts`
- Delete: `frontend/src/lib/utils/treemap.test.ts`

**Interfaces:**

- Consumes: existing heatmap store responses and the existing `TreemapItem[]`
  props.

- Produces: unchanged analytics filter calls and treemap `onSelect(id)`;
  app-owned hooks `data-heatmap-date`, `data-hour-cell`, and
  `data-treemap-id`.

- [ ] **Step 1: Add calendar and treemap behavior tests**

In `Heatmap.test.ts`, mount one non-zero day, spy on `analytics.selectDate`,
focus its app-owned cell, and press Enter:

```ts
it("selects a dated cell from the keyboard", async () => {
  analytics.heatmap = {
    metric: "messages",
    entries_from: "2026-08-17",
    levels: { l1: 1, l2: 2, l3: 3, l4: 4 },
    entries: [{ date: "2026-08-17", value: 4, level: 3 }],
  };
  const selectDate = vi
    .spyOn(analytics, "selectDate")
    .mockImplementation(() => undefined);
  const component = mount(Heatmap, { target: document.body });
  await tick();

  const cell = document.querySelector<SVGElement>(
    '[data-heatmap-date="2026-08-17"]',
  )!;
  cell.dispatchEvent(new KeyboardEvent("keydown", {
    key: "Enter",
    bubbles: true,
  }));
  expect(selectDate).toHaveBeenCalledWith("2026-08-17");
  unmount(component);
});
```

In `Treemap.test.ts`, assert the exact visible value and keyboard selection:

```ts
it("exposes tile values and activates a tile from the keyboard", async () => {
  const onSelect = vi.fn();
  const component = mount(Treemap, {
    target: document.body,
    props: {
      items: [{
        id: "project-a",
        label: "Project A",
        value: 125,
        color: "#1f77b4",
        meta: "5 sessions",
      }],
      onSelect,
      formatValue: (value: number) => `${value} tokens`,
    },
  });
  await tick();

  expect(document.body.textContent).toContain("125 tokens");
  const tile = document.querySelector<SVGGElement>(
    '[data-treemap-id="project-a"]',
  )!;
  tile.dispatchEvent(new KeyboardEvent("keydown", {
    key: "Enter",
    bubbles: true,
  }));
  expect(onSelect).toHaveBeenCalledWith("project-a");
  unmount(component);
});
```

Keep the existing hour-of-week test. Change its cell query to
`[data-hour-cell="6:0"]` so it continues to prove Sunday-first display with a
Monday-zero backend value.

- [ ] **Step 2: Run the focused tests and confirm the red state**

Run:

```bash
vp test run src/lib/components/analytics/Heatmap.test.ts src/lib/components/analytics/HourOfWeekHeatmap.test.ts src/lib/components/usage/Treemap.test.ts
```

Expected: FAIL because the app-owned cell and tile attributes do not exist.

- [ ] **Step 3: Migrate the calendar heatmap**

Map API entries to `{ date: Date, dateKey, value, level }`. Use a LayerChart
`Chart` with `c="level"`, `scaleOrdinal`, and `Calendar`. Render the cells from
the Calendar child snippet so app keyboard and click handlers remain attached:

```svelte
<Chart
  data={calendarData}
  x="date"
  c="level"
  cScale={scaleOrdinal<number, string>()
    .domain([0, 1, 2, 3, 4])
    .range(levelColors)}
  padding={{ top: 16, left: 36 }}
  height={146}
>
  <Layer>
    <Calendar
      start={calendarStart}
      end={calendarEnd}
      cellSize={16}
      monthLabel={false}
    >
      {#snippet children({ cells, cellSize })}
        {#each cells as cell (cell.data.dateKey)}
          <Rect
            x={cell.x}
            y={cell.y}
            width={cellSize[0] - 2}
            height={cellSize[1] - 2}
            fill={cell.color}
            radius={2}
            data-heatmap-date={cell.data.dateKey}
            role="button"
            tabindex="0"
            onclick={() => handleCellClick(cell.data)}
            onkeydown={(event) => handleCellKeydown(event, cell.data)}
            onpointerenter={(event) => handleCellHover(event, cell.data)}
            onpointerleave={handleCellLeave}
          />
        {/each}
      {/snippet}
    </Calendar>
  </Layer>
</Chart>
```

Generate weekday and month labels with `formatDateTime` and `getLocale()`. Keep
the clamp note, metric controls, selection outline, scroll behavior, and
application tooltip. Remove column, cell-position, SVG-width, and SVG-height
calculations.

Add a visually hidden table captioned with `m.analytics_activity_title()`. Each
row contains a localized date and the exact metric value.

- [ ] **Step 4: Migrate the hour-of-week matrix**

Keep `assignLevel`, Sunday-first `DAYS`, and the original Monday-zero indices.
Flatten the rows to `{ day, dayIdx, hour, value, level }`, then use two band
scales and one LayerChart `Bar` per cell:

```svelte
<Chart
  data={cells}
  x="hour"
  y="day"
  xScale={scaleBand<number>().domain(hours).padding(0.1)}
  yScale={scaleBand<string>().domain(dayLabels).padding(0.1)}
  padding={{ top: 18, right: 4, bottom: 4, left: 29 }}
  height={155}
>
  <Layer>
    <Axis placement="top" tickValues={hourTicks} rule={false} />
    <Axis placement="left" tickValues={dayLabels} rule={false} />
    {#each cells as cell (`${cell.dayIdx}:${cell.hour}`)}
      <Bar
        data={cell}
        fill={levelColor(cell.level)}
        data-hour-cell={`${cell.dayIdx}:${cell.hour}`}
        class:dimmed={isDimmed(cell.dayIdx, cell.hour)}
        role="button"
        tabindex="0"
        onclick={() => handleCellClick(cell.dayIdx, cell.hour)}
        onkeydown={(event) => handleCellKeydown(event, cell)}
        onpointerenter={(event) => handleCellHover(event, cell)}
        onpointerleave={handleCellLeave}
      />
    {/each}
  </Layer>
</Chart>
```

Render day and hour tick labels as app-owned buttons when LayerChart's axis
labels cannot carry the current click and keyboard handlers. Position those
buttons over the band-scale centers obtained from the Chart child context. Use
localized weekday labels but keep `dayIdx` unchanged.

Add a visually hidden table captioned with the existing hour-of-week title. Each
row contains the localized weekday, localized hour, and message count.

- [ ] **Step 5: Migrate the treemap and remove the custom layout**

Build a D3 hierarchy from positive-value items. LayerChart's hierarchy `Treemap`
owns tile coordinates. Render each leaf with LayerChart `Group`, `Rect`, and
`Text` marks:

```ts
const root = $derived(hierarchy({
  id: "root",
  label: "root",
  value: 0,
  color: "transparent",
  children: items.filter((item) => item.value > 0),
}).sum((node) => node.children ? 0 : node.value));
```

```svelte
<Chart {height}>
  <Layer>
    <LayerTreemap hierarchy={root} paddingInner={2}>
      {#snippet children({ nodes })}
        {#each nodes.filter((node) => node.depth === 1) as node (node.data.id)}
          {@const tileWidth = node.x1 - node.x0}
          {@const tileHeight = node.y1 - node.y0}
          <Group
            x={node.x0}
            y={node.y0}
            data-treemap-id={node.data.id}
            role="button"
            tabindex="0"
            aria-label={m.usage_hide_from_chart({ label: node.data.label })}
            onclick={() => onSelect?.(node.data.id)}
            onkeydown={(event) => handleKey(event, node.data.id)}
          >
            <Rect width={tileWidth} height={tileHeight} radius={3} fill={node.data.color} />
            {#if tileWidth > 60 && tileHeight > 40}
              <Text x={6} y={16} value={node.data.label} width={tileWidth - 12} truncate />
              <Text x={6} y={30} value={formatValue(node.data.value)} />
            {:else if tileWidth > 40 && tileHeight > 20}
              <Text x={4} y={14} value={node.data.label} width={tileWidth - 8} truncate />
            {/if}
          </Group>
        {/each}
      {/snippet}
    </LayerTreemap>
  </Layer>
</Chart>
```

Alias the import to avoid colliding with the app component:

```ts
import { Treemap as LayerTreemap } from "layerchart/hierarchy";
```

Keep the existing white text, hover opacity, focus ring, title text, meta line,
and format callback. Delete `frontend/src/lib/utils/treemap.ts` and its layout
tests because the application no longer owns that algorithm. Do not add a test
that checks those files stay deleted.

Add a visually hidden list captioned by the current cost-attribution or
token-attribution title. Each list item contains the tile label, formatted
value, and optional metadata.

- [ ] **Step 6: Run focused checks**

Run:

```bash
vp test run src/lib/components/analytics/Heatmap.test.ts src/lib/components/analytics/HourOfWeekHeatmap.test.ts src/lib/components/usage/Treemap.test.ts
vp check
vp run check:kit-ui
```

Expected: PASS.

- [ ] **Step 7: Commit the heatmap and treemap migration**

Use `kenn:commit` to stage the three components, their tests, and deletion of
the old treemap utility. Use the subject:

```text
feat(frontend): migrate heatmaps and treemap to LayerChart
```

### Task 3: Migrate SkillTrend without losing its interaction contract

**Files:**

- Modify: `frontend/src/lib/components/analytics/SkillTrend.svelte`
- Modify: `frontend/src/lib/components/analytics/SkillTrend.test.ts`

**Interfaces:**

- Consumes: `analytics.skills.trend`, `analytics.skillsGranularity`, and
  `settings.chartPalette`.

- Produces: the existing pressed legend buttons, slider keyboard contract,
  screen-reader table, shared selected-bucket readout, and app-owned
  `data-skill-series` marks.

- [ ] **Step 1: Replace renderer-specific test queries**

Change `.series-line` color and count queries to `[data-skill-series]`. Keep the
literal palette expectations and the tests for top-six plus `Other`, pressed
legend state, pointer readout, arrow-key movement, localized dates, empty and
error states, and granularity changes. Add a one-bucket case:

```ts
it("keeps one-bucket skill series visible", async () => {
  analytics.skills = skillsResponse([{
    date: "2026-08-17",
    by_skill: { commit: 4 },
  }]);
  const component = mount(SkillTrend, { target: document.body });
  await tick();

  expect(document.querySelector(
    '[data-skill-series="commit"]',
  )).not.toBeNull();
  expect(document.querySelector("#skill-trend-data")?.textContent)
    .toContain("4");
  unmount(component);
});
```

- [ ] **Step 2: Run the focused test and confirm the red state**

Run:

```bash
vp test run src/lib/components/analytics/SkillTrend.test.ts
```

Expected: FAIL because the app-owned series attributes do not exist.

- [ ] **Step 3: Move scale and line geometry to LayerChart**

Keep top-six ranking, the `Other` fold, stable `colorMap`, hidden keys, legend
buttons, `bucketLabel`, and exact-value table. Prepare points as
`{ index, date, value, key }`, and render each visible series through
LayerChart:

```svelte
<Chart
  data={flatVisiblePoints}
  x="index"
  y="value"
  xDomain={[0, Math.max(trendEntries.length - 1, 1)]}
  yDomain={[0, null]}
  padding={{ top: 8, right: 10, bottom: 18, left: 10 }}
  height={146}
  tooltipContext={{ mode: "quadtree-x" }}
  bind:context={chartContext}
>
  <Layer>
    <Axis placement="bottom" tickValues={labelIndexes} format={formatIndexLabel} rule />
    {#each visiblePlotSeries as series (series.key)}
      {#if series.points.length > 1}
        <Line
          data={series.points}
          data-skill-series={series.key}
          stroke={seriesColor(series.key)}
          strokeWidth={2}
        />
      {:else}
        <Circle
          data={series.points[0]}
          data-skill-series={series.key}
          fill={seriesColor(series.key)}
          r={4}
        />
      {/if}
    {/each}
    <Highlight lines points />
  </Layer>
</Chart>
```

Read the pointer bucket from `chartContext.tooltip.data?.index`. Keep a separate
`keyboardIndex`, then derive one selection:

```ts
const selectedIndex = $derived(
  keyboardIndex ?? chartContext?.tooltip.data?.index ?? null,
);
```

Drive the crosshair, live tooltip rows, `aria-valuenow`, and `aria-valuetext`
from `selectedIndex`. Clear only the pointer selection on pointer leave and only
the keyboard selection on blur. Use `getLocale()` for series totals, table
values, and tooltip values. Remove `xAt`, `yAt`, `linePath`, manual width
measurement, and the hand-written SVG.

- [ ] **Step 4: Run focused checks**

Run:

```bash
vp test run src/lib/components/analytics/SkillTrend.test.ts
vp check
vp run check:kit-ui
```

Expected: PASS.

- [ ] **Step 5: Commit the SkillTrend migration**

Use `kenn:commit` to stage the component and its test. Use the subject:

```text
feat(frontend): migrate skill trends to LayerChart
```

### Task 4: Migrate the compound concurrency timeline

**Files:**

- Modify: `frontend/src/lib/components/activity/ConcurrencyTimeline.svelte`
- Modify: `frontend/src/lib/components/activity/ConcurrencyTimeline.test.ts`

**Interfaces:**

- Consumes: the unchanged `Report`, `selectedBucket`, and `onSelectBucket`
  props.

- Produces: unchanged bucket index and localized range callback, overlay
  selector, right-axis formatting, active/idle strip, future shading, and
  pointer and keyboard selection. Marks expose `data-bucket-index` and
  `data-future` as app-owned hooks.

- [ ] **Step 1: Reframe geometry tests around named chart contracts**

Keep the current tests for selection callbacks, keyboard Enter, tooltip text,
DST-safe week ranges, minute tick labels, active/idle state, future state, and
overlay values. Replace raw mark-count and path selectors with
`[data-bucket-index]`, `[data-segment]`, and `[data-overlay-line]`. Preserve one
layout assertion that proves the automated segment sits above the interactive
segment; it protects the stacked meaning, not LayerChart internals.

Add unequal bucket durations and assert the rendered hit targets keep the same
ratio:

```ts
it("uses real bucket bounds for unequal widths", async () => {
  const report = minuteReport({
    range_end: "2026-06-16T00:15:00Z",
    buckets: [
      makeBucket("2026-06-16T00:00:00Z", "2026-06-16T00:05:00Z", 1),
      makeBucket("2026-06-16T00:05:00Z", "2026-06-16T00:15:00Z", 1),
    ],
  });
  const component = mount(ConcurrencyTimeline, {
    target: document.body,
    props: { report },
  });
  await tick();

  const slots = document.querySelectorAll<SVGElement>("[data-bucket-index]");
  const first = Number(slots[0]!.getAttribute("width"));
  const second = Number(slots[1]!.getAttribute("width"));
  expect(second / first).toBeCloseTo(2, 1);
  unmount(component);
});
```

Define `makeBucket` beside the existing report fixtures with explicit start,
end, peak fields, zero token and money values. Do not extract another helper.

- [ ] **Step 2: Run the focused test and confirm the red state**

Run:

```bash
vp test run src/lib/components/activity/ConcurrencyTimeline.test.ts
```

Expected: FAIL on the new app-owned hooks.

- [ ] **Step 3: Prepare interval and stack data without pixel geometry**

Replace `bars` with domain rows. Dates remain numeric UTC instants so scale math
does not depend on the browser timezone:

```ts
interface ConcurrencySegment {
  idx: number;
  kind: "interactive" | "automated";
  start: number;
  end: number;
  y0: number;
  y1: number;
}

const segments = $derived(buckets.flatMap((bucket, idx) => [
  {
    idx,
    kind: "interactive" as const,
    start: Date.parse(bucket.start),
    end: Date.parse(bucket.end),
    y0: 0,
    y1: bucket.interactive_at_peak,
  },
  {
    idx,
    kind: "automated" as const,
    start: Date.parse(bucket.start),
    end: Date.parse(bucket.end),
    y0: bucket.interactive_at_peak,
    y1: bucket.max_agents,
  },
]));
```

Keep bucket-range formatting, tick-boundary selection, overlay values, and
future interval calculation as domain logic. Remove every function that maps a
domain value to a pixel.

- [ ] **Step 4: Render the compound LayerChart**

Use a linear x scale over `[rangeStartMs, rangeEndMs]`, a primary y scale for
concurrency, and a remapped `y1` scale for the optional overlay. Render interval
bars with x and y ranges, then add the overlay line and right axis:

```svelte
<Chart
  data={segments}
  flatData={[...segments, ...overlayPoints]}
  x={[(row) => row.start, (row) => row.end]}
  xScale={scaleLinear()}
  xDomain={[rangeStartMs, rangeEndMs]}
  y={[(row) => row.y0, (row) => row.y1]}
  yDomain={[0, scale.max]}
  y1="overlayValue"
  y1Domain={[0, overlayMax]}
  padding={{ top: 10, right: rightAxisW, bottom: 38, left: 32 }}
  height={198}
>
  {#snippet children({ context })}
    <Layer>
      <Axis placement="left" grid tickValues={agentTickValues} />
      <Axis placement="bottom" tickValues={xTickValues} format={formatTickInstant} />
      {#each segments as segment (`${segment.idx}:${segment.kind}`)}
        <Bar
          data={segment}
          data-segment={segment.kind}
          fill={segment.kind === "interactive"
            ? "var(--accent-blue)"
            : "var(--accent-orange)"}
        />
      {/each}
      {#if overlayMetric !== "none"}
        <Line
          data={overlayPoints}
          y={(point) => context.y1Scale?.(point.overlayValue)}
          data-overlay-line
          class="overlay-line"
        />
        <Axis
          placement="right"
          scale={scaleLinear(
            context.y1Scale?.domain() ?? [0, 1],
            [context.height, 0],
          )}
          ticks={overlayTickValues}
          format={fmtOverlayTick}
          rule
        />
      {/if}
    </Layer>
  {/snippet}
</Chart>
```

Use a second short LayerChart with the same x domain for the active/idle strip.
Place it directly below the main chart so LayerChart computes both interval
widths from the same domain. Render future shading in both charts as an
`AnnotationRange` from `futureStartMs` to `rangeEndMs`.

Keep one transparent LayerChart `Bar` per bucket as the full interaction target.
Give it `data-bucket-index={idx}`, `role="button"`, `tabindex="0"`,
`aria-pressed`, and the current pointer, click, and key handlers. The callback
continues to emit only `{ idx, label }`.

Add a visually hidden table captioned with `m.activity_concurrency()`. Each row
contains the localized bucket range, interactive peak, automated peak, combined
peak, agent-minutes, output tokens, and cost.

- [ ] **Step 5: Run focused checks**

Run:

```bash
vp test run src/lib/components/activity/ConcurrencyTimeline.test.ts
vp check
vp run check:kit-ui
```

Expected: PASS.

- [ ] **Step 6: Commit the concurrency migration**

Use `kenn:commit` to stage the component and its test. Use the subject:

```text
feat(frontend): migrate concurrency timeline to LayerChart
```

### Task 5: Verify the complete migration in tests and the browser

**Files:**

- Modify only an owning chart component or its existing behavior test if a
  verification failure reveals an in-scope parity problem.

**Interfaces:**

- Consumes: the four migration commits and the existing test fixture server.

- Produces: evidence that the Analytics, Usage, Trends, and Activity pages keep
  their current behavior in light, dark, desktop, and narrow layouts.

- [ ] **Step 1: Run the full frontend verification suite**

Run from `frontend/`:

```bash
vp check
vp test
vp run check:kit-ui
vp build
```

Expected: all commands exit 0. If a command fails, use
`superpowers:systematic-debugging` before changing code or expectations.

- [ ] **Step 2: Run the relevant browser suite**

Run:

```bash
vp run e2e -- e2e/usage.spec.ts e2e/appearance-a11y.spec.ts e2e/navigation.spec.ts
```

Do not create an end-to-end test only to prove that LayerChart renders an SVG.

- [ ] **Step 3: Inspect all four pages at two widths and two themes**

Start the repository's documented local fixture server and use
`browser:control-in-app-browser`. Check these page and viewport pairs:

```text
/analytics  1440x900 and 390x844  light and dark
/usage      1440x900 and 390x844  light and dark
/trends     1440x900 and 390x844  light and dark
/activity   1440x900 and 390x844  light and dark
```

For each page, verify axis labels stay inside the card, series colors match the
legend, horizontal overflow remains usable, tooltips stay readable, focused
marks show a visible ring, and empty space does not replace one-point data.
Exercise the heatmap filters, treemap selection, skill legend and arrow keys,
activity bar selection, and concurrency overlay selector.

- [ ] **Step 4: Review the final diff for migration completeness**

Run:

```bash
git diff --check HEAD~4..HEAD
rg -n '<svg|<path|<rect|<line|<circle' src/lib/components/activity/ConcurrencyTimeline.svelte src/lib/components/analytics/ActivityTimeline.svelte src/lib/components/analytics/Heatmap.svelte src/lib/components/analytics/HourOfWeekHeatmap.svelte src/lib/components/analytics/SkillTrend.svelte src/lib/components/trends/TrendsLineChart.svelte src/lib/components/usage/CostTimeSeriesChart.svelte src/lib/components/usage/Treemap.svelte
```

Expected: the diff check is clean. Any remaining raw SVG primitive must belong
to an application-owned accessible overlay or label that LayerChart cannot
express. Do not encode this source scan as a test.

- [ ] **Step 5: Commit only verified parity adjustments**

If browser or full-suite verification required an in-scope component or test
change, use `kenn:commit` with the subject:

```text
fix(frontend): preserve chart parity after LayerChart migration
```

If verification required no file changes, do not create an empty commit.
