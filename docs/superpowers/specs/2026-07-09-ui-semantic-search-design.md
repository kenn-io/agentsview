# Command Palette Semantic Search Design

## Summary

Add Full text, Semantic, and Hybrid search modes to the existing command
palette. Preserve the current full-text behavior, route Semantic and Hybrid
queries through the content-search API, remember the last-used mode, and
normalize all results into one compact palette presentation.

This is a command-palette feature only. In-session search remains unchanged
because the semantic-search API does not currently accept a session scope.

## Goals

- Expose Full text, Semantic, and Hybrid modes in the command palette.
- Preserve existing full-text session-name matching and relevance/recency
  sorting.
- Remember the last-used mode across palette openings and browser sessions.
- Keep one result per session, using the best-ranked match.
- Surface actionable semantic-search setup, rebuild, and transient errors.
- Use the existing kit-ui component vocabulary and all configured locales.

## Non-goals

- Adding semantic search to the in-session find bar.
- Adding a dedicated search page or advanced semantic-search filters.
- Changing the backend search contracts.
- Automatically enabling, building, or repairing the vector index.
- Automatically falling back to Full text when Semantic or Hybrid fails.
- Displaying raw semantic or hybrid scores.

## Interaction Design

The command palette adds a dedicated control row below the query input. The left
side contains `SegmentedControl` from `@kenn-io/kit-ui` with three translated
options: Full text, Semantic, and Hybrid. The application must use the shared
component directly rather than recreate its behavior or chrome with local
buttons and CSS.

Full text is the initial default for users without a stored preference. When a
user changes modes, the selected value is written to local storage and restored
the next time the palette opens. If the current query has at least three
characters, changing mode cancels pending work and immediately schedules a new
search for that query.

Full text mode retains the existing Relevance/Recency control on the right side
of the mode row. Semantic and Hybrid results are ranked by their respective
backend algorithms, so the sort control is hidden in those modes. Returning to
Full text restores the store's current full-text sort value.

The existing query threshold, 300 ms debounce, keyboard navigation, recent
session list, and result-selection behavior remain unchanged. Selecting a search
result hydrates its session, navigates to the session route, and scrolls to the
matched ordinal.

## Search Store and Data Flow

The existing search store remains the single owner of command-palette search
state. It gains:

- a `SearchMode` value: `fulltext`, `semantic`, or `hybrid`;
- local-storage loading and persistence for the selected mode;
- a palette-specific normalized result type;
- a user-visible error value; and
- mode-aware request dispatch.

Full text continues to call `GET /api/v1/search` with the query, active project
filter, 30-result limit, and current relevance/recency sort. This preserves
session-name matching, FTS grouping, and current ordering behavior.

Semantic and Hybrid call `GET /api/v1/search/content` with:

- `pattern` set to the trimmed query;
- `mode` set to `semantic` or `hybrid`;
- `X-AgentsView-Search-Intent: semantic`;
- the active project filter when present; and
- a 30-result limit.

The initial UI does not send an explicit semantic scope, so the backend's
current `all` default remains in effect.

The two API response shapes are normalized into a palette result containing:

- session ID;
- project and agent;
- matched ordinal;
- result timestamp;
- snippet;
- optional session name; and
- rank metadata used internally for ordering.

For full-text results, the result timestamp is the session end time and the
session name remains available. For semantic/hybrid matches, the result
timestamp is the matched message time and the snippet is the primary label.

Semantic and Hybrid may return multiple conversation units from one session.
Normalization walks the ranked response in order and keeps the first match for
each session ID. This preserves the highest-ranked unit while preventing a long
session from crowding other sessions out of the palette. No client-side resort
is applied after deduplication.

Every new search or mode change aborts the previous generated-client request.
Only the latest non-aborted request may update results, loading state, or error
state. Clearing or closing the palette cancels both pending debounce work and
in-flight requests.

## Result and State Presentation

The existing compact result row remains the visual base.

- Full-text results show the session name when available and the best matching
  snippet below it.
- Semantic and Hybrid results lead with the matching snippet because the
  content-search response does not contain a session display name.
- Metadata shows the project and a relative result timestamp.
- Scores are not shown because semantic similarity and hybrid reciprocal-rank
  fusion scores do not share a user-meaningful scale.

Loading and empty results continue to occupy the existing results region. An
error replaces that region while leaving the query and selected mode intact. The
error presentation contains a translated heading and the actionable detail
returned by the backend. The detail is important because semantic-unavailable
responses distinguish first-time setup, an in-progress build, a stale model
fingerprint, and an incompatible index. Technical identifiers and commands in
that backend detail remain verbatim.

The UI does not silently switch modes after an error. Users can retry by editing
the query or reselecting the mode, or switch to Full text themselves.

## Localization

Add every new static label and state message to all locales listed in
`frontend/project.inlang/settings.json`: English, Simplified Chinese,
Traditional Chinese, and Korean. Keep message key sets identical.

Localized copy includes the three mode labels, the mode-control accessible
label, and the semantic-search error heading. Dynamic backend remediation text
is transported as technical diagnostic detail rather than assembled from
translated sentence fragments.

## Error Handling

- Abort/cancellation errors remain invisible and cannot clear newer results.
- Semantic unavailable responses (`501`) show the backend's setup or rebuild
  detail.
- Transient embeddings failures (`503`) show the backend detail and preserve the
  selected mode for retry.
- Other request failures show the same localized error frame with the safest
  available generated-client error detail.
- A successful subsequent search clears the previous error.
- Empty successful responses show the normal translated no-results state.

## Testing

Store tests will exercise observable behavior with the generated service
boundary mocked:

- restoring a valid stored mode and defaulting safely for missing/invalid
  values;
- persisting mode changes;
- dispatching Full text to `/api/v1/search` with its sort;
- dispatching Semantic and Hybrid to `/api/v1/search/content` with the intent
  header and project filter;
- normalizing and deduplicating ranked content matches by session;
- canceling stale searches during query and mode changes;
- preserving actionable errors and clearing them after success; and
- retaining full-text sort while it is temporarily hidden.

Component tests will mount the real kit-ui `SegmentedControl` through the
command palette and verify:

- all three localized mode options are available;
- choosing a mode updates the store and reruns an eligible query;
- Relevance/Recency is visible only in Full text;
- loading, empty, and error states render in the results region; and
- normalized results navigate and scroll to the matched ordinal.

Validation includes Paraglide compilation, focused store and component tests,
the full frontend check and test commands, and browser-driven verification of
the real command palette. Browser verification covers switching all three modes,
running searches, observing unavailable behavior, and opening a matched session.

## Documentation

Update the command-palette section of `docs/usage.md` to describe the three
modes, remembered selection, Full text-only sorting, and semantic setup
dependency. Update `docs/semantic-search.md` to remove the no-frontend
limitation and describe the command-palette integration and its command-line
configuration prerequisite.
