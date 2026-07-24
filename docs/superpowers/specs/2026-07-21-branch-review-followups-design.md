# Branch review follow-ups design

## Context

The branch-filtering PR exposes Activity and Usage breakdowns by project and
branch. Three same-head review findings remain:

- sanitizing project labels can collapse branch rows from distinct private
  projects onto the same display identity;
- the branch metadata documentation describes an earlier response shape that was
  replaced within this unshipped PR;
- usage summaries aggregate and render every branch even when the user is not
  viewing the Branch grouping.

The `/api/v1/branches` endpoint is introduced by this PR and does not exist on
`main`, so there is no shipped response contract to preserve.

## Goals

- Preserve distinct branch identity without exposing raw private project labels.
- Keep branch-row filtering exact when different projects use the same branch
  name.
- Remove branch aggregation and payload cost from ordinary Usage requests.
- Keep explicit Branch views usable for archives with thousands of branches.
- Preserve SQLite, PostgreSQL, and DuckDB behavior parity.
- Make the branch metadata documentation match the bounded search API.

## Non-goals

- Do not add a compatibility endpoint or dual response shape for an API that has
  not shipped.
- Do not cap the branch catalog or make lower-cost branches impossible to
  select.
- Do not change the existing branch-name picker contract.
- Do not expose raw path-like project labels in API responses or frontend state.

## Branch identity

Activity `BranchKeyMinutes`, daily Usage `BranchBreakdown`, and range-wide
`BranchTotal` rows gain a `project_key` field. Sanitization derives the opaque
project key from the raw project label before replacing that label with its safe
display form.

Backend aggregation remains keyed by the raw `(project, branch)` pair. Service
folding changes to key branch totals by `(project_key, branch)`, so two private
projects with the same sanitized label and branch remain separate.

The frontend uses `(project_key, branch)` for keyed rendering and selection.
When a Usage attribution row is selected, its branch token carries the opaque
project key. The service resolves project-key components back to exact raw
labels inside the local data boundary before constructing storage predicates.
Plain branch-name filters and legacy raw project-qualified tokens remain
accepted because they are current PR behavior, not a compatibility adapter.

## Lazy branch summaries

Usage summary requests gain an opt-in `branch_breakdowns` boolean. It defaults
to false. The existing `breakdowns` flag continues to control project, model,
agent, and machine data.

Each storage backend includes `git_branch` in its scan, accumulator key, daily
maps, and serialized breakdowns only when `branch_breakdowns` is true. The
service folds `branchTotals` only for those responses.

The Usage store requests the ordinary summary without branch data. Because the
time-series and attribution groupings are synchronized, switching either panel
to Branch triggers one summary refresh with `branch_breakdowns=true`. The
branch-rich summary is retained while filters are unchanged, so switching away
and back is instant. A later date or filter refresh includes branch data only
when Branch is the active grouping.

Existing query progress, abort, stale-response suppression, and error handling
remain responsible for the focused refresh.

## Virtualized attribution rows

The treemap remains capped at 40 tiles. Its side rail and the list view continue
to expose every returned branch, but render through the repository's existing
TanStack virtualizer instead of creating one DOM subtree per branch.

Both views use fixed-height rows and their own scroll element. Non-branch
dimensions may use the same rendering path for consistency, but virtualization
must not change sorting, percentages, tooltips, click actions, keyboard focus,
or visible copy.

## Branch metadata documentation

`docs/session-api.md` will describe `/api/v1/branches` as a bounded,
case-insensitive branch-name search with optional project and relationship scope
filters, a maximum page size of 100, and a `has_more` indicator. It will not
instruct clients to obtain opaque project-qualified filter tokens from this
endpoint.

## Validation

- Activity sanitization tests cover two raw projects that sanitize to the same
  label and assert distinct `project_key` values and row identities.
- SQLite, PostgreSQL, and DuckDB Usage tests cover `project_key` on branch
  breakdowns, exact project-key branch filtering, default omission, and opt-in
  inclusion.
- Service tests cover branch-total folding by `(project_key, branch)` and
  project-key token resolution, including unknown keys.
- Frontend tests cover lazy refresh on the first Branch selection, reuse of an
  already branch-rich summary, and virtualized rendering for a large branch
  set.
- Existing full Go, frontend, localization, kit-ui, benchmark, and CI checks
  remain required before completion.
