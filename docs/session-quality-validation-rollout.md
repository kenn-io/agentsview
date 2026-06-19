# Session Quality Validation And Rollout

Phase 6 validates the deterministic scoring and Insights stack before broad
rollout. Final calibration is intentionally gated on Phases 2-5 because the
signal names, API fields, deterministic Insights views, and opt-in LLM insight
flow must be stable before weights can be judged against real archives.

## Rollout Gate

Do not promote the new prompt, context, or workflow penalties as final default
semantics until all of these are true:

- Phase 2 scorer contracts are frozen, including signal names and penalty caps.
- Phase 3 persistence/API fields are additive, nullable/backfilled, and covered
  by resync preservation tests.
- Phase 4 deterministic Insights views expose scored facts before generated
  recommendation text.
- Phase 5 LLM canned insights remain opt-in, cacheable, and separate from
  canonical health score writes.
- A local archive calibration pass has reviewed score deltas, threshold
  crossings, and known false positives.

## Validation Matrix

| Area                      | Evidence                                                                                                                                   | Current scaffold                                                                        |
| ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------- |
| Score deltas              | Compare canonical score against a baseline with Coach-derived heuristics removed. Lock expected score, grade, basis, penalties, and delta. | `internal/signals/testdata/score_golden.json` and `TestComputeHealthScore_GoldenDeltas` |
| Explanation fields        | Every score delta has inspectable `Basis` and `Penalties` entries.                                                                         | Golden score assertions                                                                 |
| Archive preservation      | Full resync preserves source-missing/orphaned sessions and their computed quality signal columns.                                          | `TestResyncAllPreservesTrashedSessionData` orphan signal assertion                      |
| Saved insights            | Full resync preserves saved insight rows.                                                                                                  | `TestResyncAllPreservesInsights`                                                        |
| Deterministic Insights UI | Saved quality recommendations render provenance metadata and deterministic-score disclaimer.                                               | `frontend/e2e/insights-quality.spec.ts`                                                 |
| LLM-disabled state        | Read-only or unavailable generation disables the Generate action and avoids background mutation.                                           | `frontend/e2e/insights-quality.spec.ts`                                                 |

## Reference Repo Gap Checklist

The `/tmp/AI-Engineering-Coach` reference flow has broader validation around
analyzer contracts, page-level anti-pattern behavior, rule inventory, and reload
stability. Before final rollout, add or explicitly waive these lanes:

| Area                          | Why it matters                                                                                                                                                                                                                                                                                                               | Suggested evidence                                                                                                                                                |
| ----------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Aggregate quality contracts   | Coach validates analyzer outputs for empty data, prompt maturity, spec-driven behavior, intent classification, migration readiness, and recommendation structure. Agentsview has quality pattern transform tests, but rollout should pin the full `GetAnalyticsSignals` shape across empty, clean, and high-signal archives. | Backend SQLite and PG analytics-signals tests covering grade buckets, prompt/context/workflow/tool totals, trend buckets, and agent/project breakdowns.           |
| False-positive corpus         | Score goldens lock expected deltas, but calibration needs explicit negative cases so terse expert prompts, planning-only discussions, pure Q&A, and code tasks with enough file/tool context are not penalized incorrectly.                                                                                                  | Golden fixtures with both positive and negative sessions per score-affecting signal.                                                                              |
| Rule-to-signal inventory      | Coach has rule loader/compiler tests that catch catalog drift. Agentsview maps Coach rules into deterministic signals and analytics-only lanes, so the map should fail closed when a scored signal is undocumented or untested.                                                                                              | A lightweight docs/test check that every score-affecting `QualitySignals` field appears in the heuristic map, validation matrix, and at least one golden fixture. |
| Deterministic dashboard e2e   | Coach e2e exercises anti-pattern score cards, pattern lists, context-management cards, empty states, and drill-downs. Current Phase 6 e2e mainly covers saved/generated insight behavior.                                                                                                                                    | Playwright coverage for the Quality Patterns section: summary cards, grade distribution, pattern cards, empty/error/loading states, and filter/date changes.      |
| Calibration artifact          | The current template explains what to review, but there is no executable command that produces the review table.                                                                                                                                                                                                             | Local-only script or command that emits JSON/CSV with baseline score, new score, delta, grade crossing, top penalties, and high-delta session IDs.                |
| Reload/performance guard      | Coach includes parser/analyzer memory and reload benchmarks. Quality scoring runs during resync/backfill and analytics queries, so large archives need a runtime guard before default-on rollout.                                                                                                                            | Documented benchmark command with archive size, resync/backfill duration, analytics-signals latency, and memory notes.                                            |
| Parser/source diversity       | Reference tests cover multiple log sources before analyzer logic consumes them. Agentsview should ensure new quality signals are stable across the supported agents, not only Claude-shaped fixtures.                                                                                                                        | Signal fixtures from Codex, Claude, Cursor, Gemini, Kiro/OpenCode, and imported remote sessions where source formats differ.                                      |
| LLM insight replay boundaries | Phase 5 keeps generated insights separate from canonical scoring, but rollout should validate cache/provenance edge cases.                                                                                                                                                                                                   | Tests for malformed or partial provenance, cache-hit vs fresh generation metadata, stale template versions, provider unavailable, and read-only archives.         |

## Signal/API Contract Checklist

Every analytics signal or persisted quality-signal change must update these
surfaces together before review:

- SQLite aggregation, signal evidence queries, and example extraction.
- PostgreSQL aggregation/evidence parity, including stable timestamp formatting
  for frontend deep links and evidence ordering.
- `QualitySignals` versioning plus non-destructive stale-row recompute behavior
  for archives that already completed the legacy backfill marker.
- Server endpoint validation. Unknown `/analytics/signal-sessions` signals
  should return `400` instead of silently producing empty evidence.
- Frontend API types, pattern driver ids, labels, and evidence-panel state keyed
  by the active analytics filters.
- Contract tests for empty data, unsupported signals, filtered evidence, and at
  least one positive example per score-affecting signal.
- Documentation updates in this file and the heuristic map whenever semantics,
  confidence, or score-affecting status changes.

Outcome cohorts (`outcome_errored`, `outcome_abandoned`, `outcome_completed`)
are valid drilldown groups, but they are not calibrated diagnostic signals
because the calibration target already includes incomplete outcomes.

## Calibration Report Template

Run this after Phases 2-5 are finalized against a representative local archive.
Keep the report local unless the data has been scrubbed.

| Metric                       | Value | Notes                                                                            |
| ---------------------------- | ----- | -------------------------------------------------------------------------------- |
| Archive date range           | TBD   | Include machines/agents sampled.                                                 |
| Sessions scored              | TBD   | Exclude unscored low-confidence unknown sessions.                                |
| Median score delta           | TBD   | New score minus baseline without Coach-derived heuristics.                       |
| p90 absolute delta           | TBD   | Review high-delta sessions manually.                                             |
| Newly below A/B/C thresholds | TBD   | Count threshold crossings by grade.                                              |
| Top penalty contributors     | TBD   | Rank by frequency and total points.                                              |
| False-positive themes        | TBD   | Include examples such as terse expert prompts or planning-only code discussions. |
| Runtime impact               | TBD   | Compare resync/backfill duration before and after signal computation.            |
| Rollout decision             | TBD   | Default-on, preview-only, or shadow-only.                                        |

## Operator Notes

- New quality signal columns are additive; existing archives must not be deleted
  or recreated to adopt them.
- Full resync can recompute parser-derived signals and should preserve orphaned,
  trashed, excluded, and saved insight data before swapping databases.
- If score semantics shift materially, expose the new penalties through a
  preview or shadow path first and document the delta in release notes.
- LLM canned insights summarize deterministic aggregates only. They must not
  write canonical score, grade, signal, or penalty rows.
