# Rolling default date range for Usage and Analytics

## Problem

The Usage view (`/usage`) and Analytics view (`/analytics`) compute their
default date range once, when the page-level Svelte store is constructed:

- `UsageStore.from = daysAgo(30)`, `UsageStore.to = today()`
  (`frontend/src/lib/stores/usage.svelte.ts`).
- `AnalyticsStore.from = daysAgo(365)`, `AnalyticsStore.to = today()`
  (`frontend/src/lib/stores/analytics.svelte.ts`).

A 5-minute refresh tick, SSE event subscription, manual refresh button, and
sidebar navigation all funnel into `fetchAll()`, but `fetchAll()` reuses the
already-stored dates. If the page stays open across midnight, the visible window
stays anchored to the day the page loaded — it does not roll forward.

The Analytics view has an additional wrinkle: `DateRangePicker.svelte` presents
preset buttons (`7d`, `30d`, `90d`, `1y`, `All`). All of these call
`analytics.setDateRange(daysAgo(N), today())`, so clicking a preset today locks
the resulting absolute dates and they too stop rolling.

## Goal

The default range and any *relative* preset selection are a **rolling window**
that follows wall-clock time:

- Open the Usage view on April 24th 2026 → range is March 25th – April 24th.
  Leave the page open; the next refetch on April 25th shows March 26th – April
  25th, automatically.
- Same for Analytics with its 365-day default. Clicking the `30d` preset
  produces a rolling 30-day window. Clicking `1y` produces a rolling 365-day
  window.

Manually typing a custom date range, or loading a Usage URL with explicit
`from`/`to` query parameters, *pins* the range so it stays fixed.

## Non-goals

- No in-page UI button to reset back to rolling once pinned. Reload semantics
  differ by page (see "Transitions to rolling" below): bare `/usage` and bare
  `/analytics` always reload as rolling; a Usage URL containing `from`/`to`
  reloads as pinned (the URL itself is the pin marker). Clicking a relative
  preset on the Analytics page after a manual edit also returns to rolling,
  since presets clear the pin.
- No change to filter persistence (project/agent/model exclusions still persist
  via localStorage as today).
- No change to the existing 5-minute refresh interval, SSE subscription, or any
  other timer.
- No backend changes — the backend already accepts arbitrary `from`/`to`.

## Behavior contract

Each store has two modes, governed by a transient `isPinned: boolean` flag:

- **Rolling (default, `isPinned = false`):** before every `fetchAll()`, `from`
  is re-derived as `daysAgo(windowDays)` and `to` as `today()`, both in the
  user's local timezone. Reactive UI bound to the store (the date inputs and,
  for Usage only, the URL write-back effect) picks up the new values
  automatically.

- **Pinned (`isPinned = true`):** dates are treated as user-fixed and are not
  re-derived. `fetchAll()` runs unchanged.

Each store also tracks `windowDays: number` — the rolling window size in days.
For Usage it stays at 30 (no preset UI). For Analytics it starts at 365 and
updates whenever a relative preset is clicked.

### Transitions to pinned

- The user manually edits a date input (custom typed value). Both stores'
  `setDateRange(from, to)` setter handles this.
- The Usage page is loaded with a URL containing `from` or `to` query parameters
  (e.g. `/usage?from=2026-03-25&to=2026-04-24`). Either parameter being present
  is sufficient. If only one is supplied, the missing side keeps the
  constructor's default for that single load and the store stays pinned
  thereafter.
- The Analytics page's `All` preset (which uses the earliest-session timestamp,
  not a relative window) pins. See the implementation sketch.

The Analytics view has no URL-driven date-range entry point today; this design
does not add one.

### Transitions to rolling

- A bare URL load (`/usage`, `/analytics`) constructs the store with
  `isPinned = false`. Reloading a bare URL also returns to rolling.
- The Analytics page has no URL contract for dates today, so reloading any
  Analytics URL re-runs the constructor and starts unpinned (the 365-day rolling
  default).
- Reloading a Usage URL that contains `from` or `to` query parameters does
  **not** return to rolling: the constructor runs unpinned, then the URL-init
  `$effect` immediately re-pins from the URL params. To fully un-pin a Usage
  view, the user removes `from`/`to` from the URL bar (or navigates to a bare
  `/usage` link).
- Clicking a *relative* preset on the Analytics page (`7d`, `30d`, `90d`, `1y`)
  sets `windowDays = N`, leaves/clears `isPinned = false`, and re-derives the
  dates immediately.
- There is no in-page UI button for un-pinning on the Usage page in v1.

## URL contract (Usage page only)

The URL write-back `$effect` in `UsagePage.svelte` writes `from`/`to` query
parameters **only when `isPinned` is true**. In rolling mode the URL stays bare
for those two parameters. Other parameters (`exclude_project`, etc.) continue to
be written as today.

URL semantics:

- `/usage` → rolling default. A recipient of this link sees their own rolling
  30-day window.
- `/usage?from=X&to=Y` → pinned to the exact range the sender chose.

`AnalyticsPage.svelte` has no URL `$effect` for dates today and this design does
not add one.

## Implementation sketch

### Stores (UsageStore, AnalyticsStore)

Add to each store:

In `UsageStore`:

```ts
isPinned: boolean = $state(false);   // transient, not persisted
windowDays: number = $state(30);

private rollDates(): void {
  if (this.isPinned) return;
  this.from = daysAgo(this.windowDays);
  this.to = today();
}
```

In `AnalyticsStore`, identical except `windowDays = $state(365)`.

`fetchAll()` calls `this.rollDates()` as its first step, before the existing
filter-save and parallel fetch logic.

In each store's existing `setDateRange(from, to)` setter, add
`this.isPinned = true;` as the first line. Preserve the existing body unchanged
— for Usage that's just `from`/`to` assignments and `fetchAll()`; for Analytics
that additionally resets `selectedDate`, `selectedDow`, and `selectedHour`.

Add a new setter to `AnalyticsStore` only (Usage has no preset UI and no call
site that needs it):

```ts
setRollingWindow(days: number) {
  this.windowDays = days;
  this.isPinned = false;
  this.rollDates();
  this.fetchAll();
}
```

Setting `isPinned` consistently before the dates inside each setter keeps
`rollDates()` inside `fetchAll()` predictable — pinned setters suppress
re-derivation, rolling setters allow it.

### DateRangePicker.svelte (Analytics)

`applyPreset(days)` changes:

- For relative presets (`days > 0`): call `analytics.setRollingWindow(days)`.
  This sets the window length, clears the pin, and refetches with rolled dates.
- For the `All` preset (`days === 0`): keep current behavior —
  `analytics.setDateRange(allFrom(), todayStr())`. This pins because `allFrom()`
  is anchored to a specific historical timestamp.

`isActive(days)` keeps its current "current dates match the preset arithmetic"
check; it continues to work because rolling presets keep
`from === daysAgo(days)` and `to === today()` over time.

Custom date input edits in DateRangePicker continue to call
`analytics.setDateRange(...)`, which now also pins.

### UsagePage.svelte (URL handling)

In the URL-init `$effect`: when `params.from` or `params.to` is present, set
`usage.isPinned = true` as part of the same change batch (alongside the existing
`usage.from = ...` / `usage.to = ...` assignments).

In the URL-write-back `$effect`, also read `usage.isPinned` at the top of the
effect (alongside the existing `usage.from` / `usage.to` / `usage.exclude*`
reads) so the effect tracks pin changes. Then gate the `from`/`to` entries on
the captured value:

```ts
const from = usage.from;
const to = usage.to;
const isPinned = usage.isPinned;   // tracked: forces re-run on pin change
const exProj = usage.excludedProjects;
const exAgent = usage.excludedAgents;
const exModel = usage.excludedModels;
untrack(() => {
  if (router.route !== "usage") return;
  const params: Record<string, string> = {};
  if (isPinned) {
    if (from) params["from"] = from;
    if (to) params["to"] = to;
  }
  // existing exclude_* writes unchanged
  router.replaceParams(params);
});
```

Adding `isPinned` to the tracked reads is required so an `isPinned` flip that
does not coincide with a `from`/`to` change still re-runs the effect and resyncs
the URL.

The Usage date inputs already call `usage.setDateRange(...)` on `onchange`; that
path now also pins via the setter change above.

### Manual refresh, SSE, interval tick

No changes. They all call `fetchAll()`, which now re-derives dates first when
not pinned.

## Edge cases

| Case                                                         | Behavior                                                                                                   |
| ------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------- |
| Usage page open across midnight, default range               | Next `fetchAll()` (5-min tick, SSE event, focus, etc.) rolls the window                                    |
| Analytics page open across midnight after `30d` preset click | Next `fetchAll()` rolls the 30-day window                                                                  |
| User manually picks a custom date, then leaves open          | Stays pinned; window does not roll                                                                         |
| User loads `/usage?from=...&to=...`                          | Pinned to that range                                                                                       |
| User loads bare `/usage`                                     | Rolling; URL stays bare                                                                                    |
| Analytics user clicks `All` preset                           | Pinned to `(earliest_session, today)`; does not roll. Re-clicking refreshes against the latest `allFrom()` |
| User clicks a relative preset after a manual edit            | Returns to rolling with the new window length                                                              |
| User clicks "Clear filters" (Usage)                          | Unchanged — only clears project/agent/model exclusions                                                     |
| Reload of bare `/usage` or bare `/analytics`                 | Returns to rolling default (constructor starts unpinned)                                                   |
| Reload of `/usage?from=...&to=...`                           | Stays pinned (URL-init `$effect` re-pins from URL params after construction)                               |
| Reload of any Analytics URL                                  | Returns to rolling (Analytics has no URL contract for dates)                                               |
| Time-zone change while page is open                          | Re-derived dates use the user's current local timezone via `Date`                                          |

## Testing

Extend `frontend/src/lib/stores/usage.test.ts` and
`frontend/src/lib/stores/analytics.test.ts` (using `vi.useFakeTimers()` +
`vi.setSystemTime()` for clock control):

- Constructor produces `isPinned === false` and the expected `windowDays` (30 /
  365).
- `fetchAll()` in unpinned state recomputes `from` and `to` against the current
  "now" — advance the system clock by one day, call `fetchAll()`, assert both
  dates shift by one day.
- `setDateRange(from, to)` flips `isPinned` to `true`; subsequent `fetchAll()`
  calls (across simulated date rolls) leave `from`/`to` untouched.
- `setRollingWindow(days)` on `AnalyticsStore` sets `windowDays`, clears
  `isPinned`, and re-derives dates immediately. (Not exposed on `UsageStore` —
  no call site.)

Extract the URL-params construction from the write-back `$effect` in
`UsagePage.svelte` into a small pure helper (e.g. `buildUsageUrlParams` in
`frontend/src/lib/stores/usage.svelte.ts` or a sibling util module) that takes
`{ from, to, isPinned, excludedProjects, excludedAgents, excludedModels }` and
returns `Record<string, string>`. Unit-test it directly: assert `from`/`to` are
omitted when `isPinned === false` and included when `true`, and that the
exclude-\* keys behave as before. No equivalent test is needed for analytics —
that page has no URL effect.

No Playwright changes required.

## Out of scope

- Persisting `isPinned` or `windowDays` to localStorage. (For Usage, pinned
  state is implicitly carried across reloads via the URL's `from`/`to` params —
  the URL is the only pin-survival mechanism. For Analytics, pinned state is
  purely transient and lost on reload.)
- Adding a "Reset to default" or "Last 30 days" button to the Usage page.
- Changing the default window lengths (still 30 for Usage, 365 for Analytics).
- Adding a URL contract for the Analytics page.
- Re-deriving the `All` preset's `from` on every fetch. It pins for now; the
  user can re-click `All` to refresh.
