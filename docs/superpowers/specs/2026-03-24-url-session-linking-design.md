# URL-Based Session Linking

**Issue**: [#180](https://github.com/wesm/agentsview/issues/180)
**Date**: 2026-03-24

## Goal

Allow users of a hosted AgentsView instance to share links and navigate
directly to a session and/or message in the application.

## URL Structure

| URL                              | View                                  |
| -------------------------------- | ------------------------------------- |
| `/sessions`                      | Session list + analytics              |
| `/sessions?project=foo`          | Session list with filters             |
| `/sessions/{id}`                 | Session selected, messages visible    |
| `/sessions/{id}?msg=5`           | Scrolled to message ordinal 5         |
| `/sessions/{id}?msg=last`        | Scrolled to the last message          |
| `/insights`                      | Insights page                         |
| `/pinned`                        | Pinned messages page                  |
| `/trash`                         | Trash page                            |
| `/settings`                      | Settings page                         |

The `msg` query parameter supports:

- **Numeric ordinal** (`?msg=5`): scroll to the message at that ordinal.
- **`last`** (`?msg=last`): scroll to the final message in the session.

Filter query params (`project`, `machine`, `agent`, `include_one_shot`,
etc.) remain on `/sessions` as they work today, without the `#/` prefix.

## Approach: Additive URL Sync

The existing `sessions.activeSessionId` remains the source of truth for
session selection. URL syncing is a new layer added on top.

### What stays the same

- `RouterStore` continues to own page-level routing
  (sessions/insights/pinned/trash/settings).
- `sessions.selectSession()`, `deselectSession()`,
  `navigateToSession()` all work as before.
- All existing callers are unchanged.

### What changes

#### 1. Router migration from hash to path

`parseHash()` becomes `parsePath()`. The `hashchange` listener becomes
`popstate`. `window.location.hash = ...` becomes
`history.pushState(...)`. Same shape, different API.

#### 2. Session ID in URL

The router parses `/sessions/{id}` and exposes a reactive `sessionId`
field. Page-only routes (`/insights`, etc.) have `sessionId = null`.

#### 3. URL sync effect (App.svelte)

Two-way sync between `sessions.activeSessionId` and the URL:

- **State to URL**: when `activeSessionId` changes (sidebar click,
  keyboard nav, etc.), call `history.pushState` to update the URL to
  `/sessions/{id}` or `/sessions` (if deselected).
- **URL to State**: on initial load and `popstate` (back/forward), if
  the URL contains a session ID, call
  `sessions.navigateToSession(id)`. If the URL has `?msg=...`, queue
  the scroll via `ui.scrollToOrdinal()`.

#### 4. `msg` param handling

On load/popstate, parse `?msg=last` or `?msg={ordinal}` and trigger
scroll. `msg=last` resolves after messages load by watching
`messages.messages.length`.

## Detailed Behavior

### Initial page load with `/sessions/{id}?msg=last`

1. Router parses path: `route: "sessions"`, `sessionId: "abc"`,
   `params: { msg: "last" }`.
2. Session list loads normally via `sessions.load()`.
3. URL sync effect sees `router.sessionId` is set and calls
   `sessions.navigateToSession("abc")` (handles sessions not in the
   loaded list by fetching from API).
4. `msg=last` is stored as pending; once messages finish loading,
   resolve "last" to the final ordinal and call
   `ui.scrollToOrdinal()`.

### Selecting a session via sidebar click

1. `sessions.selectSession(id)` sets `activeSessionId` as today.
2. URL sync effect detects the change and calls
   `history.pushState(null, "", "/sessions/{id}")`.
3. No `msg` param added.

### Deselecting a session

1. `sessions.deselectSession()` sets `activeSessionId = null`.
2. URL sync effect calls
   `history.pushState(null, "", "/sessions")` preserving active
   filter params.

### Browser back/forward

1. `popstate` fires, router re-parses the URL.
2. If `sessionId` changed, sync effect selects or deselects.
3. If `msg` param is present, queue scroll.

### Navigating to a different page

1. `router.navigate("insights")` pushes `/insights`.
2. `router.sessionId` becomes `null`; session is deselected.

### Filter params

- When on `/sessions` (no session selected), filter changes update
  query params via `replaceState` (avoids polluting history).
- When a session is selected at `/sessions/{id}`, filters are not in
  the URL.

### basePath

- Read from the `<base href>` tag (already injected by the Go server
  for reverse proxy support).
- Strip from pathname when parsing; prepend when constructing
  pushState URLs.

### Session not found

If `/sessions/{id}` targets a nonexistent session,
`navigateToSession` fails silently and no session is selected. The
user sees the analytics/empty state.

## Files to Change

| File                                          | Change                                              |
| --------------------------------------------- | --------------------------------------------------- |
| `frontend/src/lib/stores/router.svelte.ts`    | Hash to path migration, add `sessionId` field       |
| `frontend/src/App.svelte`                     | Add URL sync effect, `msg` param handling           |
| `frontend/src/lib/stores/sessions.svelte.ts`  | Minor: remove `pendingNavTarget` if no longer needed |
| Callers of `router.navigate("sessions", ...)` | Update to use path-based params                     |

No Go server changes are required; the SPA fallback handler already
serves `index.html` for all unknown paths.
