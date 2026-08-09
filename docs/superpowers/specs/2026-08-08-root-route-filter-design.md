# Root Route Filter Design

## Problem

The frontend treats `/` as the Sessions route. On startup, the sessions store
restores its saved project and filter state from local storage, and the App
URL-sync effect writes that state back into the address bar. A bare visit to the
local server can therefore become a filtered `/sessions` URL and open a filtered
view the user did not request.

## Goals

- Make `/` an unfiltered Sessions landing route.
- Keep the browser URL at `/` when the landing view is unfiltered.
- Preserve the saved session-filter preference for later visits to `/sessions`.
- Keep explicit `/sessions?...` deep links authoritative over saved filters.
- Promote an explicit filter change made from `/` to a canonical `/sessions?...`
  URL.

## Non-goals

- Change the behavior of `/sessions` or any other top-level route.
- Remove or migrate existing filter preferences.
- Change the meaning of session filters, date ranges, or cross-panel date
  yoking.

## Design

### Root-path identity

The router will expose whether the current URL is the bare root path in addition
to its existing logical route. The flag will be initialized from the initial
pathname, updated on browser history navigation, and cleared by programmatic
navigation to another route. This keeps the current `sessions` route behavior
while giving the App a precise way to distinguish `/` from `/sessions`.

### Session state at `/`

When entering the bare root, the App will reset only the sessions store's
in-memory filters to their defaults and clear any active session selection. The
reset will not write to local storage. The initial sidebar load will also skip
filter persistence, so opening `/` does not erase the saved preference.

Root entry will not restore a saved session date range or apply the shared date
yoke to the landing view. The yoke preference itself remains available for
normal `/sessions` navigation.

### URL synchronization

The existing URL write-back remains unchanged for `/sessions` and session detail
routes. While the bare root is still unfiltered, it will not write session
filters into the URL. If a user explicitly changes a session filter from the
root landing view, the App will navigate to `/sessions` with the current
canonical filter parameters. Subsequent changes use the existing replace-state
synchronization.

### Compatibility and failure handling

Unknown paths that currently fall back to the Sessions route will retain their
existing behavior; only the literal bare root receives the new landing-route
semantics. Back/forward navigation will run the same root-entry reset as a fresh
visit. Existing deep links continue to initialize filters from their URL
parameters before loading sessions.

## Testing

Add frontend coverage for the observable contracts:

- A saved project filter is ignored when the App starts at `/`.
- The URL remains exactly `/` after root startup, and the saved local-storage
  value remains intact.
- A filter change from `/` navigates to `/sessions` with the filter encoded.
- `/sessions` still restores the saved filter, and an explicit
  `/sessions?project=...` link still wins over it.
- Router history/popstate updates preserve the root-path distinction.

Use the existing Vitest/jsdom seams and concrete URL, filter, and local-storage
assertions; no browser or server behavior needs to be mocked outside the
frontend boundary.
