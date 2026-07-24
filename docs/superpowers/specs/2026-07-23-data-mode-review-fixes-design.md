# Data Mode Review Fixes

## Goal

Make PR #1166 mergeable by repairing its verified correctness, upgrade,
cross-platform, publication-scope, user-feedback, and performance regressions
without changing the core Data mode architecture.

## Decisions

### Merged project inventory rows

Keep one inventory row per sanitized display label. Add every underlying project
identity key to the row while retaining `project_key` as the stable canonical
key. Candidate lookup validates that the requested key belongs to the displayed
row, then includes sessions for every raw project merged into that row. The
frontend resolves deep links against both the canonical key and the complete key
list.

This preserves the intentional privacy-safe label merge while making every
counted session reachable.

### Filtered PostgreSQL pushes

Do not publish worktree mappings during project-filtered pushes. Mapping rules
are archive-scoped configuration and cannot be partitioned safely by project:
dynamic rules have no fixed project, and independently filtered cursors share
the same archive key in PostgreSQL. Publishing a partial set could either leak
unrelated paths or delete rules belonging to another filtered scope.

Unfiltered pushes continue to publish the complete mapping set.

### Session eligibility

Data inventory, candidate discovery, preview, and apply all operate on visible
sessions (`deleted_at IS NULL`), including sessions with zero messages. This
keeps counts consistent across the three screens.

### Failed reloads

A failed foreground reload keeps the last successful inventory visible but also
renders the error and retry action. Background refresh failures continue to
preserve the current view without interrupting the user.

### Incremental append performance

Return the post-mapping project from the existing single-session mapping path so
`writeIncremental` does not perform a second `GetSession` point read. The
benchmark gate remains unchanged; the implementation must recover enough
allocations to pass it.

## Other corrections

- Repair the PostgreSQL session INSERT placeholder sequence.
- Move the source-snapshot upgrade gate to data version 71 and cover a v70
  source archive.
- Normalize persisted mapping-path expectations in cross-platform tests.
- Chunk inventory-machine and aggregate-rebuild `IN` queries.
- Map writer shutdown to the established 503 response.
- Map legacy-row uniqueness conflicts to the duplicate sentinel.
- Include `original_project` in reclassification tokens.
- Split the oversized mapping evaluator.
- Fail consistently on malformed inventory timestamps.
- Use byte-order collation for PostgreSQL mapping ordering.
- Correct stale comments and localized archive-wide candidate copy.
- Rewrite the PR description as a concise rationale-first summary.

## Validation

Use behavior-level regression tests before implementation. Run targeted Go,
PostgreSQL integration, frontend, localization, formatting, vet, and benchmark
checks, followed by the broadest practical suites. Scrub publication text and
the final diff before pushing.
