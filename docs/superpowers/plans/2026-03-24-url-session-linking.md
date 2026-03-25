# URL-Based Session Linking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps
> use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add path-based URL routing so users can share direct links to
sessions and messages (e.g. `/sessions/{id}?msg=last`).

**Architecture:** Migrate the existing hash-based router to use the
History API (pushState/popstate). Add a two-way sync effect in
App.svelte that reflects `sessions.activeSessionId` into the URL and
vice versa. The `msg` query parameter supports numeric ordinals and
`last`.

**Tech Stack:** Svelte 5 (runes), TypeScript, Vitest (jsdom), History
API

**Spec:**
`docs/superpowers/specs/2026-03-24-url-session-linking-design.md`

---

## File Map

| File | Change | Responsibility |
| --- | --- | --- |
| `frontend/src/lib/stores/router.svelte.ts` | Rewrite | Hash→path parsing, pushState navigation, `sessionId` field |
| `frontend/src/lib/stores/router.test.ts` | Rewrite | Tests for `parsePath`, `RouterStore` with History API |
| `frontend/src/App.svelte` | Modify | URL sync effect, `msg` param handling, initial deep link |
| `frontend/src/lib/components/analytics/TopSessions.svelte` | Modify | Replace `pendingNavTarget` pattern with `selectSession` |
| `frontend/src/lib/components/pinned/PinnedPage.svelte` | Modify | Replace `pendingNavTarget` pattern |
| `frontend/src/lib/components/content/SubagentInline.svelte` | Modify | Replace `pendingNavTarget` pattern, fix href |
| `frontend/src/lib/components/layout/AppHeader.svelte` | Modify | Update navigate calls |
| `frontend/src/lib/components/layout/ThreeColumnLayout.svelte` | Modify | Update navigate call |
| `frontend/src/lib/components/analytics/AgentComparison.svelte` | Modify | Update navigate call |
| `frontend/src/lib/components/analytics/SessionShape.svelte` | Modify | Update navigate call |
| `frontend/src/lib/utils/keyboard.ts` | Modify | Update navigate/route checks |
| `frontend/src/lib/utils/keyboard.test.ts` | Modify | Update test assertions |
| `frontend/src/lib/stores/sessions.svelte.ts` | Modify | Remove `pendingNavTarget` |

---

### Task 1: Rewrite `parsePath` and its tests

**Files:**

- Modify: `frontend/src/lib/stores/router.svelte.ts`
- Modify: `frontend/src/lib/stores/router.test.ts`

This task replaces `parseHash()` with `parsePath()`. The new function
reads `window.location.pathname` and `window.location.search` instead
of `window.location.hash`. It also extracts a `sessionId` from
`/sessions/{id}` paths.

A `getBasePath()` helper reads the `<base href>` tag (injected by the
Go server for reverse-proxy setups) and strips it from the pathname
before parsing.

- [ ] **Step 1: Write failing tests for `parsePath`**

Replace the `parseHash` describe block in `router.test.ts` with tests
for `parsePath`:

```typescript
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
} from "vitest";
import {
  parsePath,
  RouterStore,
} from "./router.svelte.js";

function setURL(path: string) {
  window.history.replaceState(null, "", path);
}

describe("parsePath", () => {
  afterEach(() => {
    setURL("/");
  });

  it("returns default route for root path", () => {
    setURL("/");
    const result = parsePath();
    expect(result.route).toBe("sessions");
    expect(result.sessionId).toBeNull();
    expect(result.params).toEqual({});
  });

  it("parses /sessions with query params", () => {
    setURL("/sessions?project=myproj&machine=laptop");
    const result = parsePath();
    expect(result.route).toBe("sessions");
    expect(result.sessionId).toBeNull();
    expect(result.params).toEqual({
      project: "myproj",
      machine: "laptop",
    });
  });

  it("parses /sessions/{id}", () => {
    setURL("/sessions/abc-123");
    const result = parsePath();
    expect(result.route).toBe("sessions");
    expect(result.sessionId).toBe("abc-123");
    expect(result.params).toEqual({});
  });

  it("parses /sessions/{id} with msg param", () => {
    setURL("/sessions/abc-123?msg=5");
    const result = parsePath();
    expect(result.route).toBe("sessions");
    expect(result.sessionId).toBe("abc-123");
    expect(result.params).toEqual({ msg: "5" });
  });

  it("parses /sessions/{id} with msg=last", () => {
    setURL("/sessions/abc-123?msg=last");
    const result = parsePath();
    expect(result.sessionId).toBe("abc-123");
    expect(result.params).toEqual({ msg: "last" });
  });

  it("parses page routes", () => {
    for (const route of [
      "insights",
      "pinned",
      "trash",
      "settings",
    ]) {
      setURL(`/${route}`);
      const result = parsePath();
      expect(result.route).toBe(route);
      expect(result.sessionId).toBeNull();
    }
  });

  it("falls back to default for unknown routes", () => {
    setURL("/unknown");
    const result = parsePath();
    expect(result.route).toBe("sessions");
    expect(result.sessionId).toBeNull();
  });

  it("strips basePath from pathname", () => {
    const base = document.createElement("base");
    base.href = "/agentsview/";
    document.head.appendChild(base);
    try {
      setURL("/agentsview/sessions/abc");
      const result = parsePath();
      expect(result.route).toBe("sessions");
      expect(result.sessionId).toBe("abc");
    } finally {
      base.remove();
    }
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd frontend && npx vitest run src/lib/stores/router.test.ts`
Expected: FAIL — `parsePath` is not exported

- [ ] **Step 3: Implement `parsePath` and `getBasePath`**

Replace the `parseHash` function in `router.svelte.ts`:

```typescript
type Route =
  | "sessions"
  | "insights"
  | "pinned"
  | "trash"
  | "settings";

const VALID_ROUTES: ReadonlySet<string> = new Set<Route>([
  "sessions",
  "insights",
  "pinned",
  "trash",
  "settings",
]);

const DEFAULT_ROUTE: Route = "sessions";

function getBasePath(): string {
  const base = document.querySelector("base");
  if (!base) return "";
  const href = base.getAttribute("href") ?? "";
  // Normalize: strip trailing slash, keep leading
  return href.replace(/\/+$/, "");
}

export function parsePath(): {
  route: Route;
  sessionId: string | null;
  params: Record<string, string>;
} {
  const basePath = getBasePath();
  let pathname = window.location.pathname;
  if (basePath && pathname.startsWith(basePath)) {
    pathname = pathname.slice(basePath.length);
  }
  if (!pathname.startsWith("/")) pathname = "/" + pathname;

  const segments = pathname
    .split("/")
    .filter((s) => s.length > 0);
  const routeStr = segments[0] ?? "";
  const route: Route = VALID_ROUTES.has(routeStr)
    ? (routeStr as Route)
    : DEFAULT_ROUTE;

  // Extract session ID from /sessions/{id}
  const sessionId =
    route === "sessions" && segments.length >= 2
      ? segments[1]!
      : null;

  const params = Object.fromEntries(
    new URLSearchParams(window.location.search),
  );

  return { route, sessionId, params };
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd frontend && npx vitest run src/lib/stores/router.test.ts`
Expected: `parsePath` tests PASS

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/stores/router.svelte.ts \
        frontend/src/lib/stores/router.test.ts
git commit -m "feat: replace parseHash with parsePath for path-based routing (#180)"
```

---

### Task 2: Migrate RouterStore to History API

**Files:**

- Modify: `frontend/src/lib/stores/router.svelte.ts`
- Modify: `frontend/src/lib/stores/router.test.ts`

Migrate `RouterStore` from hash-based to History API: `popstate` instead
of `hashchange`, `pushState`/`replaceState` instead of setting
`window.location.hash`. Add reactive `sessionId` field. Keep the same
public API shape (`route`, `params`, `navigate()`).

- [ ] **Step 1: Write failing tests for migrated RouterStore**

Replace the `RouterStore` describe block in `router.test.ts`:

```typescript
describe("RouterStore", () => {
  let store: RouterStore;

  afterEach(() => {
    store?.destroy();
    setURL("/");
  });

  it("initializes with parsed path", () => {
    setURL("/sessions?project=test");
    store = new RouterStore();
    expect(store.route).toBe("sessions");
    expect(store.params).toEqual({ project: "test" });
    expect(store.sessionId).toBeNull();
  });

  it("initializes sessionId from path", () => {
    setURL("/sessions/abc-123");
    store = new RouterStore();
    expect(store.route).toBe("sessions");
    expect(store.sessionId).toBe("abc-123");
  });

  it("falls back to default on invalid route", () => {
    setURL("/bogus");
    store = new RouterStore();
    expect(store.route).toBe("sessions");
  });

  it("navigate updates URL via pushState", () => {
    setURL("/");
    store = new RouterStore();
    const spy = vi.spyOn(window.history, "pushState");
    store.navigate("insights");
    expect(spy).toHaveBeenCalled();
    expect(store.route).toBe("insights");
    spy.mockRestore();
  });

  it("navigate returns false on same URL (no-op)", () => {
    setURL("/sessions");
    store = new RouterStore();
    const result = store.navigate("sessions");
    expect(result).toBe(false);
  });

  it("navigate with params builds query string", () => {
    setURL("/");
    store = new RouterStore();
    store.navigate("sessions", { project: "foo" });
    expect(window.location.pathname).toBe("/sessions");
    expect(window.location.search).toBe("?project=foo");
  });

  it("navigateToSession updates URL to /sessions/{id}", () => {
    setURL("/sessions");
    store = new RouterStore();
    store.navigateToSession("abc-123");
    expect(window.location.pathname).toBe("/sessions/abc-123");
    expect(store.sessionId).toBe("abc-123");
  });

  it("navigateToSession with msg param", () => {
    setURL("/sessions");
    store = new RouterStore();
    store.navigateToSession("abc-123", { msg: "last" });
    expect(window.location.pathname).toBe("/sessions/abc-123");
    expect(window.location.search).toBe("?msg=last");
  });

  it("navigateFromSession returns to /sessions", () => {
    setURL("/sessions/abc-123");
    store = new RouterStore();
    store.navigateFromSession();
    expect(window.location.pathname).toBe("/sessions");
    expect(store.sessionId).toBeNull();
  });

  it("navigateFromSession preserves filter params", () => {
    setURL("/sessions/abc-123");
    store = new RouterStore();
    store.navigateFromSession({ project: "myproj" });
    expect(window.location.pathname).toBe("/sessions");
    expect(window.location.search).toBe("?project=myproj");
  });

  it("responds to popstate events", () => {
    setURL("/sessions");
    store = new RouterStore();
    // Simulate navigating then going back
    setURL("/insights");
    window.dispatchEvent(new PopStateEvent("popstate"));
    expect(store.route).toBe("insights");
  });

  it("destroy removes popstate listener", () => {
    setURL("/");
    const addSpy = vi.spyOn(window, "addEventListener");
    store = new RouterStore();
    const registeredCb = addSpy.mock.calls.find(
      ([event]) => event === "popstate",
    )?.[1];
    addSpy.mockRestore();

    const removeSpy = vi.spyOn(
      window,
      "removeEventListener",
    );
    store.destroy();
    expect(removeSpy).toHaveBeenCalledWith(
      "popstate",
      registeredCb,
    );
    removeSpy.mockRestore();
  });

  it("replaceParams uses replaceState", () => {
    setURL("/sessions");
    store = new RouterStore();
    const spy = vi.spyOn(window.history, "replaceState");
    store.replaceParams({ project: "bar" });
    expect(spy).toHaveBeenCalled();
    expect(window.location.search).toBe("?project=bar");
    spy.mockRestore();
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd frontend && npx vitest run src/lib/stores/router.test.ts`
Expected: FAIL — `navigateToSession`, `navigateFromSession`,
`replaceParams`, `sessionId` don't exist yet

- [ ] **Step 3: Rewrite RouterStore implementation**

Replace the `RouterStore` class in `router.svelte.ts`:

```typescript
export class RouterStore {
  route: Route = $state("sessions");
  params: Record<string, string> = $state({});
  sessionId: string | null = $state(null);
  #onPopState: () => void;

  constructor() {
    const initial = parsePath();
    this.route = initial.route;
    this.params = initial.params;
    this.sessionId = initial.sessionId;

    this.#onPopState = () => {
      const parsed = parsePath();
      this.route = parsed.route;
      this.params = parsed.params;
      this.sessionId = parsed.sessionId;
    };
    window.addEventListener("popstate", this.#onPopState);
  }

  destroy() {
    window.removeEventListener("popstate", this.#onPopState);
  }

  #buildUrl(
    path: string,
    params: Record<string, string> = {},
  ): string {
    const basePath = getBasePath();
    const qs = new URLSearchParams(params).toString();
    const full = basePath + path;
    return qs ? `${full}?${qs}` : full;
  }

  navigate(
    route: Route,
    params: Record<string, string> = {},
  ): boolean {
    const url = this.#buildUrl(`/${route}`, params);
    if (
      url ===
      window.location.pathname + window.location.search
    ) {
      return false;
    }
    this.route = route;
    this.params = params;
    this.sessionId = null;
    window.history.pushState(null, "", url);
    return true;
  }

  navigateToSession(
    id: string,
    params: Record<string, string> = {},
  ) {
    const url = this.#buildUrl(
      `/sessions/${encodeURIComponent(id)}`,
      params,
    );
    this.route = "sessions";
    this.params = params;
    this.sessionId = id;
    window.history.pushState(null, "", url);
  }

  navigateFromSession(
    params: Record<string, string> = {},
  ) {
    const url = this.#buildUrl("/sessions", params);
    this.route = "sessions";
    this.params = params;
    this.sessionId = null;
    window.history.pushState(null, "", url);
  }

  /** Update query params without creating a history entry. */
  replaceParams(params: Record<string, string>) {
    const path = this.sessionId
      ? `/sessions/${encodeURIComponent(this.sessionId)}`
      : `/${this.route}`;
    const url = this.#buildUrl(path, params);
    this.params = params;
    window.history.replaceState(null, "", url);
  }
}

export const router = new RouterStore();
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd frontend && npx vitest run src/lib/stores/router.test.ts`
Expected: All PASS

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/stores/router.svelte.ts \
        frontend/src/lib/stores/router.test.ts
git commit -m "feat: migrate RouterStore from hash to History API (#180)"
```

---

### Task 3: Add URL sync effect and `msg` handling in App.svelte

**Files:**

- Modify: `frontend/src/App.svelte`
- Modify: `frontend/src/lib/stores/router.svelte.ts` (if minor
  adjustments needed)

Add two sync behaviors:

1. **State to URL**: when `sessions.activeSessionId` changes, push the
   URL to `/sessions/{id}` or `/sessions`.
2. **URL to State**: on initial load and popstate, if
   `router.sessionId` is set, select the session. If `msg` param is
   present, queue scroll.

The `msg=last` case resolves after messages finish loading by watching
`messages.messages` and resolving "last" to the final ordinal.

**Note:** The `urlSyncSuppressed` flag (introduced in Step 3) must be
declared before both effects that reference it. Declare it as a
module-level `let` at the top of the new effect block.

- [ ] **Step 1: Add URL-to-State effect for initial deep link and popstate**

In `App.svelte`, add a new effect after the existing route-change
effect (around line 185). This watches `router.sessionId` and
`router.params.msg`:

```typescript
// Deep-link: select session from URL and handle ?msg param.
$effect(() => {
  const sid = router.sessionId;
  const msgParam = router.params["msg"] ?? null;
  untrack(() => {
    if (sid) {
      if (sid !== sessions.activeSessionId) {
        sessions.navigateToSession(sid);
      }
      if (msgParam) {
        if (msgParam === "last") {
          // "last" resolves once messages load; store marker
          ui.pendingScrollOrdinal = -1; // sentinel for "last"
          ui.pendingScrollSession = sid;
        } else {
          const ordinal = parseInt(msgParam, 10);
          if (Number.isFinite(ordinal)) {
            ui.scrollToOrdinal(ordinal, sid);
          }
        }
      }
    } else if (router.route === "sessions") {
      if (sessions.activeSessionId !== null) {
        sessions.deselectSession();
      }
    }
  });
});
```

- [ ] **Step 2: Add `msg=last` resolution effect**

Add an effect that resolves the `pendingScrollOrdinal = -1` sentinel
once messages finish loading:

```typescript
// Resolve msg=last once messages are loaded.
$effect(() => {
  const pending = ui.pendingScrollOrdinal;
  const loading = messages.loading;
  const msgs = messages.messages;
  untrack(() => {
    if (pending !== -1 || loading || msgs.length === 0) return;
    const lastOrdinal = msgs[msgs.length - 1]!.ordinal;
    ui.scrollToOrdinal(lastOrdinal, ui.pendingScrollSession ?? undefined);
  });
});
```

- [ ] **Step 3: Add State-to-URL sync effect**

Add an effect that watches `sessions.activeSessionId` and pushes the
URL. Use a flag to avoid circular triggers with the URL-to-State
effect:

```typescript
// Sync active session to URL.
let urlSyncSuppressed = false;

$effect(() => {
  const activeId = sessions.activeSessionId;
  const currentUrlSessionId = router.sessionId;
  untrack(() => {
    if (urlSyncSuppressed) return;
    if (router.route !== "sessions") return;
    if (activeId === currentUrlSessionId) return;
    if (activeId) {
      router.navigateToSession(activeId);
    } else {
      router.navigateFromSession();
    }
  });
});
```

In the URL-to-State effect, wrap session selection with
`urlSyncSuppressed = true` / `false` to prevent the State-to-URL
effect from redundantly pushing:

```typescript
urlSyncSuppressed = true;
sessions.navigateToSession(sid);
urlSyncSuppressed = false;
```

- [ ] **Step 4: Update the route-change effect to skip re-init when session is in URL**

The existing effect (lines 176-185) calls `sessions.initFromParams()`
on every route change. When a session is in the URL (deep link), we
don't want `initFromParams` to clear the active session. Add a guard:

```typescript
$effect(() => {
  const _route = router.route;
  const params = router.params;
  const sid = router.sessionId;
  untrack(() => {
    // Don't reset filters when deep-linked to a session
    if (!sid) {
      sessions.initFromParams(params);
    }
    sessions.load();
    sessions.loadProjects();
    sessions.loadAgents();
  });
});
```

- [ ] **Step 5: Run the dev server and manually verify**

Run: `cd frontend && npm run dev` (in parallel with `make dev`)

Test these URLs in a browser:

- `/` — shows session list + analytics
- `/sessions` — same as above
- `/sessions/{valid-id}` — selects that session
- `/sessions/{valid-id}?msg=last` — selects and scrolls to last msg
- `/sessions/{valid-id}?msg=3` — selects and scrolls to ordinal 3
- `/insights`, `/pinned`, `/trash`, `/settings` — page routes work
- Click a session in sidebar — URL updates to `/sessions/{id}`
- Click back in breadcrumb — URL returns to `/sessions`
- Browser back/forward — navigates correctly

- [ ] **Step 6: Commit**

```bash
git add frontend/src/App.svelte
git commit -m "feat: add URL sync for session deep linking and msg param (#180)"
```

---

### Task 4: Migrate callers to path-based routing

**Files:**

- Modify:
  `frontend/src/lib/components/analytics/TopSessions.svelte`
- Modify: `frontend/src/lib/components/pinned/PinnedPage.svelte`
- Modify:
  `frontend/src/lib/components/content/SubagentInline.svelte`
- Modify: `frontend/src/lib/components/layout/AppHeader.svelte`
- Modify:
  `frontend/src/lib/components/layout/ThreeColumnLayout.svelte`
- Modify:
  `frontend/src/lib/components/analytics/AgentComparison.svelte`
- Modify:
  `frontend/src/lib/components/analytics/SessionShape.svelte`
- Modify: `frontend/src/lib/utils/keyboard.ts`
- Modify: `frontend/src/lib/utils/keyboard.test.ts`

All callers that used the `pendingNavTarget` + `router.navigate()`
pattern can be simplified. Since the URL sync effect in App.svelte now
handles State-to-URL, callers just need to select the session directly
and let the effect update the URL.

- [ ] **Step 1: Migrate TopSessions.svelte**

The `handleSessionClick` function currently uses `pendingNavTarget` to
survive the `initFromParams` reset. With URL sync, it can simply
select the session:

```typescript
function handleSessionClick(id: string) {
  if (router.route !== "sessions") {
    const params: Record<string, string> = {};
    if (analytics.includeOneShot) {
      params["include_one_shot"] = "true";
    }
    router.navigate("sessions", params);
  }
  sessions.selectSession(id);
}
```

- [ ] **Step 2: Migrate PinnedPage.svelte**

Replace `navigateToPin`:

```typescript
function navigateToPin(
  sessionId: string,
  ordinal: number,
) {
  ui.scrollToOrdinal(ordinal, sessionId);
  if (router.route !== "sessions") {
    router.navigate("sessions");
  }
  sessions.navigateToSession(sessionId);
}
```

Remove the `pendingNavTarget` import/usage (it was accessed via the
sessions store, so just stop setting it).

- [ ] **Step 3: Migrate SubagentInline.svelte**

Simplify `openAsSession`:

```typescript
async function openAsSession(e: MouseEvent) {
  e.preventDefault();
  e.stopPropagation();
  if (router.route !== "sessions") {
    router.navigate("sessions");
  }
  await sessions.navigateToSession(sessionId);
}
```

Update the anchor href from `href="#{sessionId}"` to
`href="/sessions/{sessionId}"` using a derived value:

```svelte
<a
  href="/sessions/{sessionId}"
  class="open-session-link"
  onclick={openAsSession}
  title="Open as full session"
>
```

- [ ] **Step 4: Migrate AppHeader.svelte**

The header has several places that call
`sessions.deselectSession(); router.navigate("sessions")`. These can
stay as-is since `navigate("sessions")` now uses `pushState`. The
deselect will trigger the URL sync effect, but `navigate` will also
push the URL — verify there's no double push. If there is, simplify
to just `router.navigate("sessions")` and let the URL-to-State effect
handle deselection.

Review each call site:

- Lines 76-77: Sessions button — `deselectSession()` +
  `navigate("sessions")` → keep, but URL sync handles deselect
  already; simplify to just `router.navigate("sessions")`.
- Lines 93-94: Same pattern → simplify.
- Lines 118-119: Same pattern → simplify.
- Lines 133, 146, 160, 344: Non-session routes — these are fine as-is.

- [ ] **Step 5: Migrate ThreeColumnLayout.svelte**

Read the current `mobileNav()` helper. Update the `navigate` call —
should work without changes since `navigate()` signature is the same.

- [ ] **Step 6: Migrate AgentComparison.svelte and SessionShape.svelte**

These use `router.navigate("sessions", { ...filterParams })`. The
API hasn't changed, so these should work as-is. Verify.

- [ ] **Step 7: Migrate keyboard.ts**

Update references:

- Line 212: `router.navigate("sessions")` — same API, works as-is.
- Lines checking `router.route === "sessions"` — works as-is.

- [ ] **Step 8: Update keyboard.test.ts**

Update test assertions that reference `router.navigate("sessions")` or
check hash values. Replace any `window.location.hash` assertions with
`window.location.pathname` assertions.

- [ ] **Step 9: Run all frontend tests**

Run: `cd frontend && npx vitest run`
Expected: All PASS

- [ ] **Step 10: Commit**

```bash
git add frontend/src/lib/components/ \
        frontend/src/lib/utils/keyboard.ts \
        frontend/src/lib/utils/keyboard.test.ts
git commit -m "feat: migrate all callers to path-based routing (#180)"
```

---

### Task 5: Remove `pendingNavTarget` and clean up

**Files:**

- Modify: `frontend/src/lib/stores/sessions.svelte.ts`
- Modify: `frontend/src/lib/stores/sessions.test.ts` (if tests
  reference pendingNavTarget)

- [ ] **Step 1: Remove `pendingNavTarget` from SessionsStore**

In `sessions.svelte.ts`:

- Remove line 66: `pendingNavTarget: string | null = null;`
- Remove lines 170-175 in `initFromParams()`:

```typescript
// Remove:
if (this.pendingNavTarget) {
  this.setActiveSession(this.pendingNavTarget);
  this.pendingNavTarget = null;
} else {
  this.setActiveSession(null);
}
// Replace with:
this.setActiveSession(null);
```

- [ ] **Step 2: Verify no remaining references to pendingNavTarget**

Run: `cd frontend && grep -r "pendingNavTarget" src/`
Expected: No results

- [ ] **Step 3: Run all frontend tests**

Run: `cd frontend && npx vitest run`
Expected: All PASS

- [ ] **Step 4: Commit**

```bash
git add frontend/src/lib/stores/sessions.svelte.ts \
        frontend/src/lib/stores/sessions.test.ts
git commit -m "refactor: remove pendingNavTarget, replaced by URL sync (#180)"
```

---

### Task 6: Run full test suite and manual verification

**Files:** None (verification only)

- [ ] **Step 1: Run Go tests**

Run: `make test-short`
Expected: All PASS (no Go changes, but verify nothing broke)

- [ ] **Step 2: Run frontend unit tests**

Run: `cd frontend && npx vitest run`
Expected: All PASS

- [ ] **Step 3: Run lint and vet**

Run: `make lint && make vet`
Expected: Clean

- [ ] **Step 4: Build full binary**

Run: `make build`
Expected: Build succeeds with embedded frontend

- [ ] **Step 5: Manual smoke test with built binary**

Start the built binary and verify:

- Direct URL `/sessions/{id}` loads the session
- `?msg=last` scrolls to last message
- `?msg=5` scrolls to ordinal 5
- Sidebar click updates URL
- Back/forward navigation works
- `/insights`, `/pinned`, `/trash`, `/settings` all work
- Filters on `/sessions?project=foo` work
- Session deselect returns URL to `/sessions`

- [ ] **Step 6: Final commit if any fixes needed**
