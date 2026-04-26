# Rolling Default Date Range Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the Usage and Analytics views' default date range a rolling
window that follows wall-clock time, while preserving manual pinning via the
date inputs and (Usage only) URL `from`/`to` params.

**Architecture:** Each store gets a transient `isPinned` flag (default `false`)
and a `windowDays` field. A new `rollDates()` method runs at the top of
`fetchAll()` and re-derives `from`/`to` when not pinned. Manual date input edits
flip `isPinned` to `true`; on Analytics, relative presets get a new
`setRollingWindow` setter that keeps rolling. UsagePage's URL effects gate
`from`/`to` reads/writes on `isPinned` via an extracted `buildUsageUrlParams`
pure helper for unit testing.

**Tech Stack:** Svelte 5 runes (`$state`, `$effect`), TypeScript, Vitest with
`vi.useFakeTimers({ toFake: ["Date"] })`.

**Spec:** `docs/superpowers/specs/2026-04-25-rolling-usage-date-range-design.md`

______________________________________________________________________

## File Structure

| File                                                            | Change | Responsibility                                                                                                                                                              |
| --------------------------------------------------------------- | ------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `frontend/src/lib/stores/usage.svelte.ts`                       | Modify | Add `isPinned`, `windowDays`, `rollDates()`. Pin in `setDateRange`. Call `rollDates()` first inside `fetchAll`. Export `buildUsageUrlParams` helper.                        |
| `frontend/src/lib/stores/usage.test.ts`                         | Modify | Add a describe block for rolling default behavior; add a describe block for `buildUsageUrlParams`.                                                                          |
| `frontend/src/lib/stores/analytics.svelte.ts`                   | Modify | Add `isPinned`, `windowDays`, `rollDates()`. Pin in `setDateRange`. Call `rollDates()` first inside `fetchAll`. Add `setRollingWindow(days)` method.                        |
| `frontend/src/lib/stores/analytics.test.ts`                     | Modify | Add a describe block for rolling default behavior including `setRollingWindow`.                                                                                             |
| `frontend/src/lib/components/usage/UsagePage.svelte`            | Modify | URL-init effect: set `usage.isPinned = true` when URL has `from`/`to`. URL-write-back effect: track `usage.isPinned`; delegate param construction to `buildUsageUrlParams`. |
| `frontend/src/lib/components/analytics/DateRangePicker.svelte`  | Modify | Route relative presets (`days > 0`) to `analytics.setRollingWindow(days)`; keep `All` preset on `setDateRange(allFrom(), todayStr())`.                                      |
| `frontend/src/lib/components/analytics/DateRangePicker.test.ts` | Create | Mount the picker, click each preset, verify the right store method is invoked.                                                                                              |

______________________________________________________________________

## Task 1: UsageStore rolling support

**Files:**

- Modify: `frontend/src/lib/stores/usage.svelte.ts`

- Modify: `frontend/src/lib/stores/usage.test.ts`

- [ ] **Step 1: Add a fresh-store loader and clock setup at the top of
  `usage.test.ts`**

Open `frontend/src/lib/stores/usage.test.ts`. The file already has
`installStorage`, `loadStore`, and an `installStorage` helper at the top, plus
`vi.mock("../api/client.js", ...)` with default mocked impls. Just below the
existing `loadStore()` helper, append a new describe block at the end of the
file:

```ts
describe("UsageStore rolling default date range", () => {
  beforeEach(() => {
    installStorage();
    localStorage.removeItem("usage-toggles");
    localStorage.removeItem("usage-filters");
    vi.clearAllMocks();
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-04-25T12:00:00"));
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("constructor produces isPinned=false and windowDays=30 with rolling defaults", async () => {
    const { usage } = await loadStore();
    expect(usage.isPinned).toBe(false);
    expect(usage.windowDays).toBe(30);
    expect(usage.from).toBe("2026-03-26");
    expect(usage.to).toBe("2026-04-25");
  });
});
```

Also add `afterEach` to the imports at the top of the file:

```ts
import {
  beforeEach,
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";
```

- [ ] **Step 2: Run the failing test**

Run from the `frontend/` directory:

```bash
cd frontend && npm test -- usage.test.ts
```

Expected: the new test fails because `usage.isPinned` and `usage.windowDays` are
undefined.

- [ ] **Step 3: Add `isPinned`, `windowDays`, and `rollDates` to `UsageStore`**

In `frontend/src/lib/stores/usage.svelte.ts`, add the new state fields right
after `to`. The class currently looks like:

```ts
class UsageStore {
  from: string = $state(daysAgo(30));
  to: string = $state(today());
  // ...
```

Change it to:

```ts
class UsageStore {
  from: string = $state(daysAgo(30));
  to: string = $state(today());
  isPinned: boolean = $state(false);
  windowDays: number = $state(30);
  // ...
```

Then, anywhere convenient inside the class (e.g. just before
`async fetchAll()`), add the `rollDates` private method:

```ts
private rollDates(): void {
  if (this.isPinned) return;
  this.from = daysAgo(this.windowDays);
  this.to = today();
}
```

- [ ] **Step 4: Run the constructor-default test to verify it passes**

```bash
cd frontend && npm test -- usage.test.ts
```

Expected: the new test passes; existing tests continue to pass.

- [ ] **Step 5: Add the rolling-fetch test**

Append to the same describe block in `usage.test.ts`:

```ts
it("fetchAll re-derives from/to against the current clock while unpinned", async () => {
  const { usage } = await loadStore();

  expect(usage.from).toBe("2026-03-26");
  expect(usage.to).toBe("2026-04-25");

  vi.setSystemTime(new Date("2026-04-26T12:00:00"));
  await usage.fetchAll();

  expect(usage.from).toBe("2026-03-27");
  expect(usage.to).toBe("2026-04-26");
});
```

- [ ] **Step 6: Run the rolling-fetch test to verify it fails**

```bash
cd frontend && npm test -- usage.test.ts
```

Expected: the new test fails because `fetchAll()` does not yet call
`rollDates()`.

- [ ] **Step 7: Wire `rollDates` into `fetchAll`**

In `frontend/src/lib/stores/usage.svelte.ts`, modify `fetchAll`. The current
body is:

```ts
async fetchAll() {
  saveUsageFilters(this);
  await Promise.all([
    this.fetchSummary(),
    this.fetchTopSessions(),
  ]);
}
```

Change it to:

```ts
async fetchAll() {
  this.rollDates();
  saveUsageFilters(this);
  await Promise.all([
    this.fetchSummary(),
    this.fetchTopSessions(),
  ]);
}
```

- [ ] **Step 8: Run the rolling-fetch test to verify it passes**

```bash
cd frontend && npm test -- usage.test.ts
```

Expected: passes.

- [ ] **Step 9: Add the pin-on-setDateRange test**

Append to the describe block:

```ts
it("setDateRange pins and subsequent fetchAll does not roll", async () => {
  const { usage } = await loadStore();
  usage.setDateRange("2026-01-01", "2026-01-15");
  expect(usage.isPinned).toBe(true);
  expect(usage.from).toBe("2026-01-01");
  expect(usage.to).toBe("2026-01-15");

  vi.setSystemTime(new Date("2026-04-26T12:00:00"));
  await usage.fetchAll();

  expect(usage.isPinned).toBe(true);
  expect(usage.from).toBe("2026-01-01");
  expect(usage.to).toBe("2026-01-15");
});
```

- [ ] **Step 10: Run the pin test to verify it fails**

```bash
cd frontend && npm test -- usage.test.ts
```

Expected: fails because `setDateRange` does not yet set `isPinned`.

- [ ] **Step 11: Make `setDateRange` flip the pin**

In `usage.svelte.ts`, modify `setDateRange`:

```ts
setDateRange(from: string, to: string) {
  this.isPinned = true;
  this.from = from;
  this.to = to;
  this.fetchAll();
}
```

(Add `this.isPinned = true;` as the first line; preserve the rest.)

- [ ] **Step 12: Run all usage tests to verify everything passes**

```bash
cd frontend && npm test -- usage.test.ts
```

Expected: all tests pass.

- [ ] **Step 13: Type-check**

```bash
cd frontend && npm run check
```

Expected: no errors.

- [ ] **Step 14: Commit**

```bash
git add frontend/src/lib/stores/usage.svelte.ts frontend/src/lib/stores/usage.test.ts
git commit -m "feat(usage): rolling default date range with isPinned flag"
```

______________________________________________________________________

## Task 2: AnalyticsStore rolling support and setRollingWindow

**Files:**

- Modify: `frontend/src/lib/stores/analytics.svelte.ts`

- Modify: `frontend/src/lib/stores/analytics.test.ts`

- [ ] **Step 1: Add a fresh-store loader at the top of `analytics.test.ts`**

Open `frontend/src/lib/stores/analytics.test.ts`. Just after the existing
`mockAllAPIs()` helper (around line 165) and before the `resetStore()` helper,
add:

```ts
async function loadAnalyticsStore() {
  vi.resetModules();
  vi.clearAllMocks();
  mockAllAPIs();
  return import("./analytics.svelte.js");
}
```

Also add `afterEach` to the existing vitest imports at the top of the file:

```ts
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
} from "vitest";
```

- [ ] **Step 2: Add a describe block for rolling defaults at the end of the
  file**

Append:

```ts
describe("AnalyticsStore rolling default date range", () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-04-25T12:00:00"));
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("constructor produces isPinned=false and windowDays=365", async () => {
    const { analytics } = await loadAnalyticsStore();
    expect(analytics.isPinned).toBe(false);
    expect(analytics.windowDays).toBe(365);
    expect(analytics.from).toBe("2025-04-25");
    expect(analytics.to).toBe("2026-04-25");
  });
});
```

- [ ] **Step 3: Run to verify the test fails**

```bash
cd frontend && npm test -- analytics.test.ts
```

Expected: failure — `isPinned` / `windowDays` undefined on the store.

- [ ] **Step 4: Add `isPinned`, `windowDays`, and `rollDates` to
  `AnalyticsStore`**

In `frontend/src/lib/stores/analytics.svelte.ts`, add the new state fields right
after `to` (around line 64):

```ts
class AnalyticsStore {
  from: string = $state(daysAgo(365));
  to: string = $state(today());
  isPinned: boolean = $state(false);
  windowDays: number = $state(365);
  // ...
```

Add a private `rollDates` method anywhere in the class (e.g. right before
`async fetchAll()` near line 384):

```ts
private rollDates(): void {
  if (this.isPinned) return;
  this.from = daysAgo(this.windowDays);
  this.to = today();
}
```

- [ ] **Step 5: Run the constructor-defaults test to verify it passes**

```bash
cd frontend && npm test -- analytics.test.ts
```

Expected: the new constructor-defaults test passes; existing tests continue to
pass.

- [ ] **Step 6: Add the rolling-fetch test**

Append to the new describe block:

```ts
it("fetchAll re-derives from/to against the current clock while unpinned", async () => {
  const { analytics } = await loadAnalyticsStore();

  expect(analytics.from).toBe("2025-04-25");
  expect(analytics.to).toBe("2026-04-25");

  vi.setSystemTime(new Date("2026-04-26T12:00:00"));
  await analytics.fetchAll();

  expect(analytics.from).toBe("2025-04-26");
  expect(analytics.to).toBe("2026-04-26");
});
```

- [ ] **Step 7: Run to verify it fails**

```bash
cd frontend && npm test -- analytics.test.ts
```

Expected: failure — `fetchAll` does not yet call `rollDates`.

- [ ] **Step 8: Wire `rollDates` into `fetchAll`**

In `analytics.svelte.ts`, modify `fetchAll`:

```ts
async fetchAll() {
  this.rollDates();
  await Promise.all([
    this.fetchSummary(),
    this.fetchActivity(),
    this.fetchHeatmap(),
    this.fetchProjects(),
    this.fetchHourOfWeek(),
    this.fetchSessionShape(),
    this.fetchVelocity(),
    this.fetchTools(),
    this.fetchTopSessions(),
    this.fetchSignals(),
  ]);
}
```

(Add `this.rollDates();` as the first line; preserve the rest.)

- [ ] **Step 9: Run to verify it passes**

```bash
cd frontend && npm test -- analytics.test.ts
```

Expected: passes.

- [ ] **Step 10: Add the pin-on-setDateRange test**

Append to the describe block:

```ts
it("setDateRange pins and subsequent fetchAll does not roll", async () => {
  const { analytics } = await loadAnalyticsStore();
  analytics.setDateRange("2026-01-01", "2026-01-15");
  expect(analytics.isPinned).toBe(true);
  expect(analytics.from).toBe("2026-01-01");
  expect(analytics.to).toBe("2026-01-15");

  vi.setSystemTime(new Date("2026-04-26T12:00:00"));
  await analytics.fetchAll();

  expect(analytics.isPinned).toBe(true);
  expect(analytics.from).toBe("2026-01-01");
  expect(analytics.to).toBe("2026-01-15");
});
```

- [ ] **Step 11: Run to verify it fails**

```bash
cd frontend && npm test -- analytics.test.ts
```

Expected: failure — `setDateRange` does not pin.

- [ ] **Step 12: Make `setDateRange` flip the pin**

In `analytics.svelte.ts`, modify `setDateRange` (around line 532). Existing
body:

```ts
setDateRange(from: string, to: string) {
  this.from = from;
  this.to = to;
  this.selectedDate = null;
  this.selectedDow = null;
  this.selectedHour = null;
  this.fetchAll();
}
```

Change to:

```ts
setDateRange(from: string, to: string) {
  this.isPinned = true;
  this.from = from;
  this.to = to;
  this.selectedDate = null;
  this.selectedDow = null;
  this.selectedHour = null;
  this.fetchAll();
}
```

(Add `this.isPinned = true;` as the first line; preserve the drill-down resets.)

- [ ] **Step 13: Run to verify it passes**

```bash
cd frontend && npm test -- analytics.test.ts
```

Expected: passes.

- [ ] **Step 14: Add the `setRollingWindow` tests**

Append to the describe block:

```ts
it("setRollingWindow sets windowDays, clears the pin, and re-derives dates", async () => {
  const { analytics } = await loadAnalyticsStore();
  analytics.setDateRange("2026-01-01", "2026-01-15");
  expect(analytics.isPinned).toBe(true);

  analytics.setRollingWindow(7);

  expect(analytics.isPinned).toBe(false);
  expect(analytics.windowDays).toBe(7);
  expect(analytics.from).toBe("2026-04-18");
  expect(analytics.to).toBe("2026-04-25");
});

it("after setRollingWindow, fetchAll keeps rolling", async () => {
  const { analytics } = await loadAnalyticsStore();
  analytics.setRollingWindow(7);
  expect(analytics.from).toBe("2026-04-18");

  vi.setSystemTime(new Date("2026-04-26T12:00:00"));
  await analytics.fetchAll();

  expect(analytics.from).toBe("2026-04-19");
  expect(analytics.to).toBe("2026-04-26");
});

it("setRollingWindow clears any active drill-down (selectedDate/Dow/Hour)", async () => {
  const { analytics } = await loadAnalyticsStore();
  analytics.selectedDate = "2026-04-20";
  analytics.selectedDow = 3;
  analytics.selectedHour = 14;

  analytics.setRollingWindow(7);

  expect(analytics.selectedDate).toBeNull();
  expect(analytics.selectedDow).toBeNull();
  expect(analytics.selectedHour).toBeNull();
});
```

- [ ] **Step 15: Run to verify both fail**

```bash
cd frontend && npm test -- analytics.test.ts
```

Expected: failure — `setRollingWindow` does not exist.

- [ ] **Step 16: Add `setRollingWindow` to `AnalyticsStore`**

In `analytics.svelte.ts`, add the new method right after `setDateRange`. It must
clear the drill-down filters the same way `setDateRange` does — otherwise
clicking a relative preset while drilled into a date or hour leaves the panels
filtered to the old drill-down instead of the newly-selected rolling range.

```ts
setRollingWindow(days: number) {
  this.windowDays = days;
  this.isPinned = false;
  this.selectedDate = null;
  this.selectedDow = null;
  this.selectedHour = null;
  this.rollDates();
  this.fetchAll();
}
```

- [ ] **Step 17: Run to verify all tests pass**

```bash
cd frontend && npm test -- analytics.test.ts
```

Expected: all tests pass.

- [ ] **Step 18: Type-check**

```bash
cd frontend && npm run check
```

Expected: no errors.

- [ ] **Step 19: Commit**

```bash
git add frontend/src/lib/stores/analytics.svelte.ts frontend/src/lib/stores/analytics.test.ts
git commit -m "feat(analytics): rolling default date range with isPinned flag and setRollingWindow setter"
```

______________________________________________________________________

## Task 3: UsagePage URL handling — extract helper and wire effects

**Files:**

- Modify: `frontend/src/lib/stores/usage.svelte.ts` (add and export
  `buildUsageUrlParams`)

- Modify: `frontend/src/lib/stores/usage.test.ts` (test `buildUsageUrlParams`)

- Modify: `frontend/src/lib/components/usage/UsagePage.svelte` (URL init +
  write-back effects)

- [ ] **Step 1: Add a describe block for `buildUsageUrlParams`**

Append to `frontend/src/lib/stores/usage.test.ts`:

```ts
describe("buildUsageUrlParams", () => {
  it("omits from/to when isPinned is false, includes excludes", async () => {
    const { buildUsageUrlParams } = await loadStore();
    const params = buildUsageUrlParams({
      from: "2026-03-26",
      to: "2026-04-25",
      isPinned: false,
      excludedProjects: "p1",
      excludedAgents: "a1",
      excludedModels: "m1",
    });
    expect(params).toEqual({
      exclude_project: "p1",
      exclude_agent: "a1",
      exclude_model: "m1",
    });
  });

  it("includes from/to when isPinned is true", async () => {
    const { buildUsageUrlParams } = await loadStore();
    const params = buildUsageUrlParams({
      from: "2026-01-01",
      to: "2026-01-15",
      isPinned: true,
      excludedProjects: "",
      excludedAgents: "",
      excludedModels: "",
    });
    expect(params).toEqual({
      from: "2026-01-01",
      to: "2026-01-15",
    });
  });

  it("returns empty object when nothing is set", async () => {
    const { buildUsageUrlParams } = await loadStore();
    const params = buildUsageUrlParams({
      from: "",
      to: "",
      isPinned: false,
      excludedProjects: "",
      excludedAgents: "",
      excludedModels: "",
    });
    expect(params).toEqual({});
  });

  it("omits empty from/to even when pinned", async () => {
    const { buildUsageUrlParams } = await loadStore();
    const params = buildUsageUrlParams({
      from: "",
      to: "",
      isPinned: true,
      excludedProjects: "",
      excludedAgents: "",
      excludedModels: "",
    });
    expect(params).toEqual({});
  });
});
```

- [ ] **Step 2: Run to verify the tests fail**

```bash
cd frontend && npm test -- usage.test.ts
```

Expected: failure — `buildUsageUrlParams` is not exported.

- [ ] **Step 3: Add and export `buildUsageUrlParams` from `usage.svelte.ts`**

At the bottom of `frontend/src/lib/stores/usage.svelte.ts`, after the existing
`export const usage = new UsageStore();` line, add:

```ts
export interface UsageUrlState {
  from: string;
  to: string;
  isPinned: boolean;
  excludedProjects: string;
  excludedAgents: string;
  excludedModels: string;
}

export function buildUsageUrlParams(
  state: UsageUrlState,
): Record<string, string> {
  const params: Record<string, string> = {};
  if (state.isPinned) {
    if (state.from) params["from"] = state.from;
    if (state.to) params["to"] = state.to;
  }
  if (state.excludedProjects) {
    params["exclude_project"] = state.excludedProjects;
  }
  if (state.excludedAgents) {
    params["exclude_agent"] = state.excludedAgents;
  }
  if (state.excludedModels) {
    params["exclude_model"] = state.excludedModels;
  }
  return params;
}
```

- [ ] **Step 4: Run to verify all helper tests pass**

```bash
cd frontend && npm test -- usage.test.ts
```

Expected: all tests pass.

- [ ] **Step 5: Wire `buildUsageUrlParams` into the URL write-back `$effect` in
  `UsagePage.svelte`**

In `frontend/src/lib/components/usage/UsagePage.svelte`, locate the URL
write-back effect (currently around lines 129–144). Replace the entire effect
body so it tracks `usage.isPinned` and uses the helper.

Existing code:

```ts
$effect(() => {
  const from = usage.from;
  const to = usage.to;
  const exProj = usage.excludedProjects;
  const exAgent = usage.excludedAgents;
  const exModel = usage.excludedModels;
  untrack(() => {
    if (router.route !== "usage") return;
    const params: Record<string, string> = {};
    if (from) params["from"] = from;
    if (to) params["to"] = to;
    if (exProj) params["exclude_project"] = exProj;
    if (exAgent) params["exclude_agent"] = exAgent;
    if (exModel) params["exclude_model"] = exModel;
    router.replaceParams(params);
  });
});
```

Replace with:

```ts
$effect(() => {
  const state = {
    from: usage.from,
    to: usage.to,
    isPinned: usage.isPinned,
    excludedProjects: usage.excludedProjects,
    excludedAgents: usage.excludedAgents,
    excludedModels: usage.excludedModels,
  };
  untrack(() => {
    if (router.route !== "usage") return;
    router.replaceParams(buildUsageUrlParams(state));
  });
});
```

Add `buildUsageUrlParams` to the import from `usage.svelte.js` at the top of the
file:

```ts
import { usage, buildUsageUrlParams } from "../../stores/usage.svelte.js";
```

(The existing import is `import { usage } from "../../stores/usage.svelte.js";`
— extend it.)

- [ ] **Step 6: Update the URL-init `$effect` to sync `usage.isPinned` to URL
  truth**

In the same file, locate the URL-init effect (currently around lines 87–125).
The contract is: a URL with `from` or `to` pins the store; a URL without those
params unpins the store. This must run regardless of whether other filter keys
are present, so a sidebar navigation back to bare `/usage` or to
`/usage?exclude_project=foo` actually returns the store to rolling mode
(otherwise the URL write-back effect re-pins from the stale store state and the
user can never escape pinned mode without a full browser reload).

Existing relevant code (start of the `untrack(() => { ... })` block):

```ts
if (route !== "usage") return;
const hasFilterKeys = Object.keys(params).some(
  (k) => USAGE_FILTER_KEYS.has(k),
);
if (!hasFilterKeys) { urlInitRan = true; return; }
let changed = false;
if (params["from"] && params["from"] !== usage.from) {
  // ...
```

Replace those leading lines with:

```ts
if (route !== "usage") return;
const hasDateParam = !!params["from"] || !!params["to"];
const hasFilterKeys = Object.keys(params).some(
  (k) => USAGE_FILTER_KEYS.has(k),
);

let changed = false;

// Sync pin state from URL: dated URL pins, undated URL unpins.
// Runs before the !hasFilterKeys early return so a fully bare URL
// (no exclude_* either) still flips the pin off.
if (usage.isPinned !== hasDateParam) {
  usage.isPinned = hasDateParam;
  changed = true;
}

if (!hasFilterKeys) {
  if (changed && urlInitRan) {
    usage.fetchAll();
  }
  urlInitRan = true;
  return;
}

if (params["from"] && params["from"] !== usage.from) {
  // ...rest of existing logic unchanged
```

The remaining lines of the effect (date applies, exclude applies, the trailing
`if (changed && urlInitRan) { usage.fetchAll(); }`) stay exactly as they are.
The only structural change is moving `let changed = false;` above the
`!hasFilterKeys` early return and adding the pin-sync block plus the
early-return refetch path.

Setting `changed = true` when the pin flips ensures `fetchAll()` runs after the
initial URL-init pass (so `rollDates()` re-derives dates when transitioning to
rolling, and the new pin state takes effect immediately). The URL write-back
effect tracks `usage.isPinned` (Step 5) and will resync the URL on its own.

- [ ] **Step 7: Type-check and run the full frontend test suite**

```bash
cd frontend && npm run check && npm test
```

Expected: no type errors; all tests pass.

- [ ] **Step 8: Manual smoke verification**

Start the frontend dev server and the Go backend in separate terminals:

```bash
make dev          # in one terminal
make frontend-dev # in another
```

Then in the browser:

1. Navigate to `/usage`. Verify the URL stays bare (no `from`/`to` query
   params).
1. Pick a date in the "from" input. Verify the URL gains `?from=...&to=...`.
1. Reload the page. Verify the URL stays the same and the picked dates remain.
1. Manually edit the URL bar to remove `from` and `to`, hit enter. Verify the
   URL stays bare and the dates revert to today minus 30 days through today.

If any verification step fails, debug before proceeding.

- [ ] **Step 9: Commit**

```bash
git add frontend/src/lib/stores/usage.svelte.ts \
        frontend/src/lib/stores/usage.test.ts \
        frontend/src/lib/components/usage/UsagePage.svelte
git commit -m "feat(usage): URL pinning gate via buildUsageUrlParams helper"
```

______________________________________________________________________

## Task 4: DateRangePicker — route relative presets to setRollingWindow

**Files:**

- Modify: `frontend/src/lib/components/analytics/DateRangePicker.svelte`

- Create: `frontend/src/lib/components/analytics/DateRangePicker.test.ts`

- [ ] **Step 1: Create the failing test**

Create `frontend/src/lib/components/analytics/DateRangePicker.test.ts`:

```ts
// @vitest-environment jsdom
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
} from "vitest";
import { mount, unmount } from "svelte";

const mockAnalytics = vi.hoisted(() => ({
  setDateRange: vi.fn(),
  setRollingWindow: vi.fn(),
  from: "2026-04-25",
  to: "2026-04-25",
}));

const mockSync = vi.hoisted(() => ({
  stats: { earliest_session: "2024-01-01T00:00:00Z" },
}));

vi.mock("../../stores/analytics.svelte.js", () => ({
  analytics: mockAnalytics,
}));

vi.mock("../../stores/sync.svelte.js", () => ({
  sync: mockSync,
}));

// @ts-ignore
import DateRangePicker from "./DateRangePicker.svelte";

describe("DateRangePicker preset routing", () => {
  let target: HTMLElement;
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    vi.clearAllMocks();
    target = document.createElement("div");
    document.body.appendChild(target);
  });

  afterEach(() => {
    if (component !== undefined) {
      unmount(component);
      component = undefined;
    }
    target.remove();
  });

  function clickPreset(label: string) {
    const btn = [...target.querySelectorAll("button")].find(
      (b) => b.textContent?.trim() === label,
    );
    if (!btn) throw new Error(`preset button "${label}" not found`);
    btn.click();
  }

  it.each([
    ["7d", 7],
    ["30d", 30],
    ["90d", 90],
    ["1y", 365],
  ])(
    "relative preset %s calls setRollingWindow(%i)",
    (label, days) => {
      component = mount(DateRangePicker, { target });
      clickPreset(label);
      expect(mockAnalytics.setRollingWindow).toHaveBeenCalledWith(days);
      expect(mockAnalytics.setDateRange).not.toHaveBeenCalled();
    },
  );

  it("All preset calls setDateRange (pinned)", () => {
    component = mount(DateRangePicker, { target });
    clickPreset("All");
    expect(mockAnalytics.setDateRange).toHaveBeenCalledTimes(1);
    expect(mockAnalytics.setRollingWindow).not.toHaveBeenCalled();
  });
});
```

- [ ] **Step 2: Run to verify the tests fail**

```bash
cd frontend && npm test -- DateRangePicker.test.ts
```

Expected: failure — relative presets currently call `setDateRange`, not
`setRollingWindow`.

- [ ] **Step 3: Update `applyPreset` in DateRangePicker.svelte**

In `frontend/src/lib/components/analytics/DateRangePicker.svelte`, locate
`applyPreset` (around lines 43–49). Existing:

```ts
function applyPreset(days: number) {
  if (days === 0) {
    analytics.setDateRange(allFrom(), todayStr());
  } else {
    analytics.setDateRange(daysAgo(days), todayStr());
  }
}
```

Change to:

```ts
function applyPreset(days: number) {
  if (days === 0) {
    analytics.setDateRange(allFrom(), todayStr());
  } else {
    analytics.setRollingWindow(days);
  }
}
```

`daysAgo` and `todayStr` may now be unused. If TypeScript / svelte-check warns
about that in the next step, delete the unused helpers (and any other
newly-unused locals).

- [ ] **Step 4: Run the test and svelte-check**

```bash
cd frontend && npm test -- DateRangePicker.test.ts && npm run check
```

Expected: tests pass; no type or svelte-check warnings. Remove any newly-unused
helpers if check flags them.

- [ ] **Step 5: Manual smoke verification**

With both dev servers running, in the browser:

1. Navigate to `/analytics`.
1. Click `30d` preset. Verify the date range becomes `(today - 30 days)` to
   `today`.
1. Leave the page open until midnight (or use browser DevTools to mock the
   clock); verify the next refetch tick rolls the range forward by one day.
1. Click `All` preset. Verify the range becomes `(earliest_session)` to `today`.

If midnight verification is impractical, simulate by setting
`vi.setSystemTime`-equivalent in DevTools, or skip that bullet and trust the
unit tests.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/components/analytics/DateRangePicker.svelte \
        frontend/src/lib/components/analytics/DateRangePicker.test.ts
git commit -m "feat(analytics): route DateRangePicker relative presets to setRollingWindow"
```

______________________________________________________________________

## Final verification

- [ ] **Run the full frontend test suite**

```bash
cd frontend && npm run check && npm test
```

Expected: all tests pass; no type errors.

- [ ] **Run e2e (optional, but recommended)**

```bash
make e2e
```

Expected: existing E2E tests still pass. No new E2E coverage is added by this
plan.
