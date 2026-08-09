# Root Route Filter Design

## Problem

The frontend treats `/` as the Sessions route. On startup, the sessions store
restores its saved project and filter state from local storage, and the App
URL-sync effect writes that state back into the address bar. A bare visit to the
local server can therefore become a filtered `/sessions` URL and open a filtered
view the user did not request.

## Goals

- Make the root path `/` an unfiltered Sessions landing route, allowing
  non-filter query parameters such as the desktop sticky parameter.
- Keep the browser URL at `/` plus its sticky parameters when the landing view
  is unfiltered.
- Preserve the saved session-filter preference for later visits to `/sessions`.
- Keep explicit `/sessions?...` deep links authoritative over saved filters.
- Keep filter-param deep links such as `/?project=...` authoritative in the same
  way as `/sessions?project=...`.
- Promote any session-filter state that diverges from defaults while at the
  unfiltered root to a canonical `/sessions?...` URL.

## Non-goals

- Change the behavior of `/sessions` or any other top-level route.
- Remove or migrate existing filter preferences.
- Change the meaning of session filters, date ranges, or cross-panel date
  yoking.

## Design

### Root-path identity

The router will expose whether the current URL has the root path, independent of
its query string, in addition to its existing logical route. The flag will be
initialized from the initial pathname after the configured base path, updated on
browser history navigation, and cleared by any programmatic navigation method,
including `navigate`, `replace`, `navigateToSession`, and `replaceParams`. This
keeps the current `sessions` route behavior while giving the App a precise way
to distinguish `/` from `/sessions`, even when sticky parameters are present.

The App will define the unfiltered root landing state as the root path with no
session-filter parameters. Thus `/?desktop=1` is still the unfiltered landing
route, while `/?project=...` is an explicit filter deep link and is handled like
`/sessions?project=...`.

### Session state at `/`

When entering the unfiltered root, the App will reset only the sessions store's
in-memory filters to their defaults and clear any active session selection. The
reset will not write to local storage. While the App remains in this root state,
filter persistence is suppressed for every sessions load, including live-refresh
and sync-triggered loads, so repeated refreshes cannot erase the saved
preference.

Root entry will not restore a saved session date range or apply the shared date
yoke to the landing view. The yoke preference itself remains available for
normal `/sessions` navigation.

When the user navigates in-app from the unfiltered root to `/sessions` without
filter parameters, the App will restore the saved session filters before the
normal `/sessions` load and date/yoke restoration. This means the Sessions nav
item acts like a return to the user's remembered Sessions view, even though a
fresh root visit is intentionally unfiltered. Explicit filter parameters on the
destination remain authoritative.

### URL synchronization

The existing URL write-back remains unchanged for `/sessions` and session detail
routes. While the root is unfiltered, it will not write session filters into the
URL. If session-filter state diverges from defaults while at the root, the App
will navigate to `/sessions` with the current canonical filter parameters; this
deliberately covers both user interactions and programmatic state changes.
Promotion clears the root flag even though both locations use the logical
`sessions` route, and re-enables filter persistence. Subsequent changes use the
existing replace-state synchronization.

Promotion uses a history-pushing navigation, so the Back button returns to the
unfiltered root and resets the in-memory filters again. Later filter changes
within `/sessions` remain replace-state updates and do not add more entries.

### Compatibility and failure handling

Unknown paths that currently fall back to the Sessions route will retain their
existing behavior; only the root path without session-filter parameters receives
the new landing-route semantics. Back/forward navigation will run the same
root-entry reset as a fresh visit. Existing `/sessions?...` and `/?...` filter
deep links continue to initialize filters from their URL parameters before
loading sessions.

## Testing

Add frontend coverage for the observable contracts:

- A saved project filter is ignored when the App starts at `/`.
- The URL remains `/` (or `/` plus a sticky desktop parameter) after root
  startup, and the saved local-storage value remains intact.
- A live refresh while at `/` does not persist the temporary default filters.
- Navigating in-app from `/` to `/sessions` restores the saved filter.
- A filter-state divergence from `/` navigates to `/sessions` with the filter
  encoded, and Back returns to the unfiltered root.
- `/sessions` still restores the saved filter, and explicit `/sessions?...` and
  `/?project=...` links still win over it.
- Router history/popstate and all programmatic navigation methods preserve the
  root-path distinction and clear it on promotion.

Use the existing Vitest/jsdom seams and concrete URL, filter, and local-storage
assertions; no browser or server behavior needs to be mocked outside the
frontend boundary.
