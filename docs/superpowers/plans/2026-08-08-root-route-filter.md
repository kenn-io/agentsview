# Root Route Filter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Track each
> step with a checkbox.

**Goal:** Make a bare local visit to '/' show an unfiltered Sessions landing
view without overwriting the saved session-filter preference.

**Architecture:** Add a reactive root-path flag to the router. The sessions
store will hold persistence while its filters are the temporary defaults created
by root entry; this hold survives session detail and live refresh, and releases
on saved-state restoration or filter divergence. App route effects will gate
date restoration and URL write-back, restore saved filters on direct '/' to
'/sessions' list navigation, and promote divergent filters to '/sessions?...'.

**Tech Stack:** Svelte 5, TypeScript, Vitest through Vite+, jsdom, existing
reactive stores, and the browser History API.

## Global Constraints

- '/' with no session-filter parameters is an unfiltered landing route; sticky
  parameters such as 'desktop' remain attached.
- '/?project=...' and '/sessions?...' are explicit filter deep links and remain
  authoritative over saved filters.
- The root-reset state must not write to local storage during any session load,
  live refresh, or session-detail navigation.
- Direct in-app '/' to bare '/sessions' restores saved filters; closing detail
  opened from '/' stays unfiltered.
- Use existing frontend test seams and assert observable URLs, filters, and
  local-storage values. Add no dependencies or localization changes.

## Files and Responsibilities

- 'frontend/src/lib/stores/router.svelte.ts' and 'router.test.ts': root-path
  parsing, reactive state, sticky parameters, popstate, and navigation
  clearing.
- 'frontend/src/lib/stores/sessions.svelte.ts' and 'sessions.test.ts':
  root-reset state, persistence hold, saved-filter restoration, and every save
  seam.
- 'frontend/src/App.svelte' and 'App.test.ts': root entry, direct list restore,
  explicit deep links, promotion, Back behavior, and session-detail
  protection.

______________________________________________________________________

### Task 1: Track root-path identity in the router

**Files:**

- Modify: 'frontend/src/lib/stores/router.svelte.ts'
- Test: 'frontend/src/lib/stores/router.test.ts'

**Interfaces:**

- 'parsePath()' returns 'isRootPath: boolean' alongside 'route', 'sessionId',
  and 'params'.

- 'RouterStore.isRootPath: boolean' is reactive and describes the pathname after
  the configured base path, independent of query parameters.

- [ ] **Step 1: Add failing tests.** Cover '/' and '/?desktop=1' as root,
  '/sessions?desktop=1' as non-root, base-path root parsing, popstate from
  '/sessions' to '/', and clearing the flag through 'navigate', 'replace',
  'navigateToSession', 'navigateFromSession', and 'replaceParams'.

```ts
it("marks root independently of query parameters", () => {
  setURL("/?desktop=1");
  expect(parsePath().isRootPath).toBe(true);
  setURL("/sessions?desktop=1");
  expect(parsePath().isRootPath).toBe(false);
});

it("updates root state on popstate", () => {
  setURL("/sessions");
  store = new RouterStore();
  setURL("/?desktop=1");
  window.dispatchEvent(new PopStateEvent("popstate"));
  expect(store.isRootPath).toBe(true);
});
```

- [ ] **Step 2: Run the focused test and verify failure.**

```bash
cd frontend
vp test run src/lib/stores/router.test.ts
```

Expected: FAIL because the parsed result and store have no root flag.

- [ ] **Step 3: Implement the router flag.** After base-path stripping and
  leading-slash normalization, set 'const isRootPath = pathname === "/"' and
  return it. Add 'isRootPath: boolean = $state(false)', initialize it from the
  constructor parse, update it in the popstate handler, and set it to 'false'
  whenever 'navigate', 'replace', 'navigateToSession', 'navigateFromSession',
  or 'replaceParams' performs a history update. Leave 'navigate''s same-URL
  no-op before that mutation.

- [ ] **Step 4: Run the focused test and verify success.**

```bash
cd frontend
vp test run src/lib/stores/router.test.ts
```

Expected: PASS.

- [ ] **Step 5: Commit the router unit.**

```bash
git add frontend/src/lib/stores/router.svelte.ts frontend/src/lib/stores/router.test.ts
git commit -m "fix(frontend): track root path in router"
```

### Task 2: Add a filter-persistence hold to the sessions store

**Files:**

- Modify: 'frontend/src/lib/stores/sessions.svelte.ts'
- Test: 'frontend/src/lib/stores/sessions.test.ts'

**Interfaces:**

- 'resetFiltersForRoot(): void' installs default in-memory filters, clears the
  active session, and starts the hold without writing local storage.

- 'restoreSavedFilters(): void' reloads local storage, installs saved filters,
  clears the hold, and clears the active session.

- 'load()' and 'applyPanelDateFilters()' keep their current signatures but use
  hold-aware persistence.

- [ ] **Step 1: Add failing storage tests.** Seed a literal version-2 payload
  with a project and agent, call 'resetFiltersForRoot()', then call 'load()'
  and 'refreshSidebarIfAttached()' after 'attachSidebar()'. Assert the filters
  are default and the original payload is unchanged after both loads. Add
  tests that 'restoreSavedFilters()' restores the project and allows the next
  load to save, that a divergent project saves, and that setting an active
  session plus calling 'deselectSession()' does not release the hold or
  overwrite storage.

```ts
it("holds saved filters through root loads and refreshes", async () => {
  const saved = JSON.stringify({
    version: 2,
    project: "saved-project",
    agent: "codex",
  });
  localStorage.setItem("session-filters", saved);
  const store = createSessionsStore();
  store.resetFiltersForRoot();

  await store.load();
  expect(store.filters.project).toBe("");
  expect(localStorage.getItem("session-filters")).toBe(saved);

  const detach = store.attachSidebar();
  store.refreshSidebarIfAttached();
  await vi.waitFor(() => {
    expect(api.getSidebarSessionIndex).toHaveBeenCalledTimes(2);
  });
  expect(localStorage.getItem("session-filters")).toBe(saved);
  detach();
});
```

- [ ] **Step 2: Run the focused test and verify failure.**

```bash
cd frontend
vp test run src/lib/stores/sessions.test.ts
```

Expected: FAIL because reset/restore methods do not exist and 'load()' always
calls 'saveFilters'.

- [ ] **Step 3: Implement hold-aware persistence.** Add a private
  'filterPersistenceHeld' boolean, a default-state predicate based on
  'Object.keys(filtersToParams(this.filters)).length === 0' plus
  'dateFiltersWindowDays === null', and this helper:

```ts
private persistFiltersIfAllowed(): void {
  if (this.filterPersistenceHeld && !this.hasDefaultSessionFilters()) {
    this.filterPersistenceHeld = false;
  }
  if (!this.filterPersistenceHeld) {
    saveFilters(this.filters, this.dateFiltersWindowDays);
  }
}
```

Use it in both 'load()' and 'applyPanelDateFilters()'. Implement
'resetFiltersForRoot()' with the same cache invalidation and active-session
reset semantics as 'clearSessionFilters()', but set the hold and do not save.
Implement 'restoreSavedFilters()' with a fresh 'loadSavedFilters()' call, cache
invalidation, active-session reset, and hold release. Make 'initFromParams()'
release the hold because URL filter state is explicit. This ensures timer, sync,
live-refresh, and date-panel paths all use the same guard.

- [ ] **Step 4: Run the focused test and verify success.**

```bash
cd frontend
vp test run src/lib/stores/sessions.test.ts
```

Expected: PASS.

- [ ] **Step 5: Commit the sessions-store unit.**

```bash
git add frontend/src/lib/stores/sessions.svelte.ts frontend/src/lib/stores/sessions.test.ts
git commit -m "fix(frontend): hold saved filters during root session view"
```

### Task 3: Gate App route effects and protect user-visible flows

**Files:**

- Modify: 'frontend/src/App.svelte'
- Test: 'frontend/src/App.test.ts'

**Interfaces:**

- 'rootLanding' is 'router.isRootPath && !hasFilterParams(router.params)'.

- A previous-root flag distinguishes direct root-to-list navigation from
  root-to-detail navigation.

- Root entry calls 'resetFiltersForRoot()', direct root-to-bare-list calls
  'restoreSavedFilters()', and root-opened detail leaves the hold active.

- [ ] **Step 1: Add failing App tests.** Use the existing startup stubs and
  'flushEffects()' helper. Assert that a saved project is cleared in memory at
  '/' while the exact local-storage payload and pathname remain unchanged;
  that '/?desktop=1' remains root; that '/?project=explicit-project'
  initializes the explicit filter and normalizes to
  '/sessions?project=explicit-project'; that direct
  'router.navigate("sessions")' restores saved filters; and that a divergent
  root filter pushes '/sessions?...' and returns to unfiltered state on root
  popstate.

Add the review regression: after mounting at '/', call
'router.navigateToSession("session-a")', flush effects, trigger the real store
load/refresh seam, call 'sessions.deselectSession()' and
'router.navigateFromSession()', flush again, and assert the saved local-storage
payload is unchanged and 'sessions.filters.project === ""'.

- [ ] **Step 2: Run the focused App test and verify failure.**

```bash
cd frontend
vp test run src/App.test.ts
```

Expected: FAIL because App currently writes restored filters into '/', does not
distinguish root-to-list from root-to-detail, and has no hold coordination.

- [ ] **Step 3: Implement root-aware route initialization.** In the existing
  '$effect.pre', track 'router.isRootPath', compute 'rootLanding', and retain
  'previousRootLanding'. Treat a transition as direct root-to-list only when
  the previous state was root landing, the current route is 'sessions', the
  current 'sessionId' is null, and no session-filter params are present. On
  root entry, reset filters and skip 'sessionEntryDateParams'/yoke
  restoration. On direct root-to-list, restore saved filters before the
  existing date/yoke restoration and treat it as an 'enteringSessions'
  transition. Keep explicit 'initFromParams()' handling unchanged apart from
  its hold release. The root-to-detail transition must load defaults without
  restoring saved filters.

- [ ] **Step 4: Implement root-aware URL write-back.** In the existing write-
  back effect, return without URL mutation while 'rootLanding' has empty
  serialized session-filter params. When those params become non-empty, call
  'router.navigateToSessions(newParams)' so promotion pushes '/sessions?...',
  clears the root flag, and lets the store's divergence check release the
  hold. Keep existing detail and normal '/sessions' replace-state behavior
  unchanged.

- [ ] **Step 5: Run the focused integration tests and verify success.**

```bash
cd frontend
vp test run src/App.test.ts src/lib/stores/router.test.ts src/lib/stores/sessions.test.ts
```

Expected: PASS, including startup, sticky root, explicit deep links, direct list
restoration, promotion/Back, session detail, and persistence regressions.

- [ ] **Step 6: Commit the App integration.**

```bash
git add frontend/src/App.svelte frontend/src/App.test.ts
git commit -m "fix(frontend): keep root sessions landing unfiltered"
```

### Task 4: Run repository verification and hand off

**Files:** Verify the three implementation files and their colocated tests.

- [ ] **Step 1: Run the full frontend test suite.**

```bash
cd frontend
vp test run
```

Expected: PASS.

- [ ] **Step 2: Run frontend formatting, lint, and type checks.**

```bash
cd frontend
vp check
```

Expected: PASS with no new formatting, lint, or type errors.

- [ ] **Step 3: Check the final diff and worktree.**

Run 'git diff --check', 'git status --short --branch', and 'git log --oneline
-5'. Expected: only focused root-route commits are present and no unrelated
files are modified.
