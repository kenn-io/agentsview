# Collapsible Calls Detail Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an accessible disclosure control to the session analysis Calls
section and persist its expanded state across sessions and reloads.

**Architecture:** Extend the existing global `UIStore` with one boolean
preference, initialized and persisted through the store's guarded LocalStorage
pattern. Bind the Calls header in `SessionVitals.svelte` to that preference and
conditionally render only the detail body, leaving the localized section label
and summary visible in both states.

**Tech Stack:** Svelte 5 runes, TypeScript, Vitest/Vite+, Testing Library,
Paraglide messages, Lucide Svelte icons.

## Global Constraints

- The disclosure defaults to expanded when no valid preference exists.
- The preference is global to the analysis sidebar, not scoped to a session.
- The collapsed header retains the existing localized Calls summary.
- No new user-facing copy or locale message keys are introduced.
- The control is keyboard reachable, exposes `aria-expanded`, and has a visible
  focus state.
- Tests assert rendered behavior and the owned LocalStorage boundary.

______________________________________________________________________

### Task 1: Persisted Calls Disclosure

**Files:**

- Modify: `frontend/src/lib/stores/ui.svelte.ts`
- Modify: `frontend/src/lib/stores/ui.test.ts`
- Modify: `frontend/src/lib/components/content/SessionVitals.svelte`
- Modify: `frontend/src/lib/components/content/SessionVitals.test.ts`
- Modify: `frontend/e2e/session-timing.spec.ts`

**Interfaces:**

- Consumes: existing `readStoredBool(key, fallback)`, `UIStore` persistence
  effects, `m.session_vitals_calls()`, and
  `m.session_vitals_calls_summary(...)`.

- Produces: `ui.vitalsCallsExpanded: boolean` and
  `ui.toggleVitalsCalls(): void`, consumed by `SessionVitals.svelte`.

- [ ] **Step 1: Add failing store tests for restoration and persistence**

Add a `describe("Calls detail preference", ...)` block to
`frontend/src/lib/stores/ui.test.ts`. The restoration test imports a fresh store
module with `agentsview-session-vitals-calls-expanded` seeded to `"false"` and
expects `vitalsCallsExpanded` to be false. The persistence test toggles a fresh
store instance and expects the key to be written as `"false"`.

```ts
describe("Calls detail preference", () => {
  it("restores a collapsed Calls detail preference", async () => {
    const original = globalThis.localStorage;
    const setItem = vi.fn();
    Object.defineProperty(globalThis, "localStorage", {
      value: {
        getItem: vi.fn((key: string) =>
          key === "agentsview-session-vitals-calls-expanded" ? "false" : null,
        ),
        setItem,
      },
      writable: true,
      configurable: true,
    });

    try {
      // @ts-expect-error -- query string busts module cache
      const mod = await import("./ui.svelte.js?vitalsCallsCollapsed");
      expect(mod.ui.vitalsCallsExpanded).toBe(false);
    } finally {
      Object.defineProperty(globalThis, "localStorage", {
        value: original,
        writable: true,
        configurable: true,
      });
    }
  });

  it("persists Calls detail changes", async () => {
    const original = globalThis.localStorage;
    const setItem = vi.fn();
    Object.defineProperty(globalThis, "localStorage", {
      value: { getItem: vi.fn(() => null), setItem },
      writable: true,
      configurable: true,
    });

    try {
      // @ts-expect-error -- query string busts module cache
      const mod = await import("./ui.svelte.js?vitalsCallsPersist");
      setItem.mockClear();
      mod.ui.toggleVitalsCalls();
      await tick();
      expect(mod.ui.vitalsCallsExpanded).toBe(false);
      expect(setItem).toHaveBeenCalledWith(
        "agentsview-session-vitals-calls-expanded",
        "false",
      );
    } finally {
      Object.defineProperty(globalThis, "localStorage", {
        value: original,
        writable: true,
        configurable: true,
      });
    }
  });
});
```

- [ ] **Step 2: Run the store tests and verify the expected failure**

Run:

```bash
cd frontend
npm test -- src/lib/stores/ui.test.ts
```

Expected: FAIL because `vitalsCallsExpanded` and `toggleVitalsCalls` do not yet
exist.

- [ ] **Step 3: Add the failing component disclosure test**

In `frontend/src/lib/components/content/SessionVitals.test.ts`, reset
`ui.vitalsCallsExpanded = true` in `beforeEach`. Add a concrete timing fixture
with one Bash call, render the component, and assert the disclosure's observable
behavior.

```ts
it("collapses and restores the Calls detail while keeping its summary", async () => {
  mocks.fetchSessionTiming.mockResolvedValue(timingWithCall());
  component = mount(SessionVitals, {
    target: document.body,
    props: { sessionId: "sess-1" },
  });
  await tick();
  await tick();

  const disclosure = document.querySelector<HTMLButtonElement>(
    `button[aria-expanded="true"]`,
  );
  expect(disclosure).not.toBeNull();
  expect(disclosure?.textContent).toContain(m.session_vitals_calls());
  expect(document.querySelector(".scale-axis")).not.toBeNull();
  expect(document.querySelector(".calls")).not.toBeNull();

  disclosure!.click();
  await tick();

  expect(disclosure?.getAttribute("aria-expanded")).toBe("false");
  expect(disclosure?.textContent).toContain(
    m.session_vitals_calls_summary({
      count: 1,
      countLabel: "1",
      runningCount: 0,
    }),
  );
  expect(document.querySelector(".scale-axis")).toBeNull();
  expect(document.querySelector(".calls")).toBeNull();

  disclosure!.click();
  await tick();

  expect(disclosure?.getAttribute("aria-expanded")).toBe("true");
  expect(document.querySelector(".scale-axis")).not.toBeNull();
  expect(document.querySelector(".calls")).not.toBeNull();
});

function timingWithCall(): SessionTiming {
  return {
    ...mocks.timing,
    tool_duration_ms: 400,
    tool_call_count: 1,
    turns: [
      {
        message_id: 1,
        ordinal: 1,
        started_at: "2026-07-14T12:00:00Z",
        duration_ms: 400,
        primary_category: "Bash",
        calls: [
          {
            tool_use_id: "call-1",
            tool_name: "Bash",
            category: "Bash",
            duration_ms: 400,
            is_parallel: false,
            input_preview: "go test ./...",
          },
        ],
      },
    ],
  };
}
```

- [ ] **Step 4: Run the component test and verify the expected failure**

Run:

```bash
cd frontend
npm test -- src/lib/components/content/SessionVitals.test.ts
```

Expected: FAIL because the Calls header is not a disclosure button and the
detail remains rendered after activation.

- [ ] **Step 5: Implement the persisted UI-store preference**

In `frontend/src/lib/stores/ui.svelte.ts`, add the storage key beside the other
analysis-panel keys, initialize the state with an expanded fallback, persist it
in the constructor, and expose the toggle.

```ts
const VITALS_CALLS_EXPANDED_KEY =
  "agentsview-session-vitals-calls-expanded";

vitalsCallsExpanded: boolean = $state(
  readStoredBool(VITALS_CALLS_EXPANDED_KEY, true),
);

$effect(() => {
  try {
    localStorage?.setItem(
      VITALS_CALLS_EXPANDED_KEY,
      String(this.vitalsCallsExpanded),
    );
  } catch {
    // ignore
  }
});

toggleVitalsCalls() {
  this.vitalsCallsExpanded = !this.vitalsCallsExpanded;
}
```

- [ ] **Step 6: Implement the Calls disclosure UI**

Import `ChevronRightIcon` in `SessionVitals.svelte`. Replace the Calls header
contents with a full-width button bound to the global preference, and render the
axis and rows only while expanded.

```svelte
<header class="v-h calls-header" class:expanded={ui.vitalsCallsExpanded}>
  <button
    type="button"
    class="calls-disclosure"
    aria-expanded={ui.vitalsCallsExpanded}
    onclick={() => ui.toggleVitalsCalls()}
  >
    <span class="calls-heading">
      <span class="calls-chevron" class:open={ui.vitalsCallsExpanded}>
        <ChevronRightIcon size="10" strokeWidth="2.4" aria-hidden="true" />
      </span>
      <span>{m.session_vitals_calls()}</span>
    </span>
    <span class="v-meta">
      {m.session_vitals_calls_summary({
        count: timing.tool_call_count,
        countLabel: formatNumber(timing.tool_call_count),
        runningCount: timing.running ? 1 : 0,
      })}
    </span>
  </button>
</header>
{#if ui.vitalsCallsExpanded}
  <!-- existing scale axis and calls list -->
{/if}
```

Add scoped styles that preserve the existing header layout while making the full
row interactive.

```css
.calls-header {
  margin-bottom: 0;
}

.calls-disclosure {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  width: calc(100% + 8px);
  padding: 2px 4px;
  margin: -2px -4px;
  border-radius: var(--radius-sm);
  color: inherit;
  text-align: left;
  transition: background 0.12s;
}

.calls-header.expanded {
  margin-bottom: 9px;
}

.calls-disclosure:hover {
  background: var(--bg-surface-hover);
}

.calls-disclosure:focus-visible {
  outline: 2px solid var(--accent-blue);
  outline-offset: 2px;
}

.calls-heading,
.calls-chevron {
  display: inline-flex;
  align-items: center;
}

.calls-heading {
  gap: 4px;
}

.calls-chevron {
  transition: transform 0.15s ease-out;
}

.calls-chevron.open {
  transform: rotate(90deg);
}
```

- [ ] **Step 7: Keep the existing section-count E2E assertion semantic**

Update `frontend/e2e/session-timing.spec.ts` so the section-header assertion
selects the four `.v-h` elements instead of assuming every header's first child
is a text-only span. This preserves the existing user-visible assertion after
Calls becomes a button.

```ts
const headers = page
  .locator(".v-section .v-h")
  .filter({ hasText: /(Session|Time spent|Timeline|Calls)/ });
await expect(headers).toHaveCount(4);
```

- [ ] **Step 8: Run the focused tests and verify they pass**

Run:

```bash
cd frontend
npm test -- src/lib/stores/ui.test.ts src/lib/components/content/SessionVitals.test.ts
```

Expected: 2 test files pass with no failures.

- [ ] **Step 9: Run frontend validation**

Run:

```bash
cd frontend
npm run check
npm run check:kit-ui
npm test
```

Expected: Svelte/TypeScript and kit-ui checks pass and all frontend tests pass.

- [ ] **Step 10: Run the focused Playwright spec when the local E2E harness is
  available**

Run:

```bash
cd frontend
npm run e2e -- session-timing.spec.ts
```

Expected: the Session Vital Signs spec passes. If the local E2E harness cannot
start, report the exact blocker instead of weakening or skipping assertions.

- [ ] **Step 11: Review and commit the implementation**

Review `git diff --check`, `git status --short`, `git diff --stat`, and
`git diff HEAD`. Stage only the five implementation/test files and commit with:

```bash
git commit -m "feat(frontend): collapse Calls detail in analysis"
```
