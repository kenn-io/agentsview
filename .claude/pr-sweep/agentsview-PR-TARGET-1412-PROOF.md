# Target 1412 proof report, round 2

Working tree: `D:\Repos\agentsview-pr-1412-copilot-long-lines`
Base commit: `0e27e4d029cd1a7ece3dda448c5695c0b5a6feb4`.
Committed HEAD: `fdfae269f`.
The focused captures below were produced from the committed working tree.

The replay path now carries one validated `vscodeCopilotReplayLimits` value.
Production callers continue through `reconstructJSONL(path)`, which supplies
the unchanged 64 MiB normal and 128 MiB hard defaults. Boundary tests inject
8 KiB and 16 KiB limits, and all generated threshold records are capped in the
low tens of KiB. Positron uses a small JSONL fixture to prove production
routing and identity without duplicating threshold allocation.

| ID | Surface | Command run | Observed result | Capture |
| --- | --- | --- | --- | --- |
| boundaries | issue-shaped, kind-1/2, exact-normal, hard-ceiling, trailing JSON, Positron JSONL | `CGO_ENABLED=1 go test -tags "fts5,kit_posthog_disabled" ./internal/parser -run 'TestReconstructJSONL(OversizedCopilotIssueShape\|OversizedKindOneAndTwo\|AtNormalLimit\|HardCeiling\|RejectsTrailingJSON)?$\|TestPositronProviderParse.*JSONL$' -count=1 -v` | PASS; kilobyte fixtures preserve indexed content and metadata, elide only `resultDetails.output`, accept the exact 8 KiB normal boundary, and reject 16 KiB plus one at the injected hard ceiling. | `file:///D:/Repos/.claude/pr-sweep/captures/agentsview-PR-TARGET-1412-review-6/focused-boundaries.log` |
| ordinary | ordinary kind-0/1/2/3 replay | `CGO_ENABLED=1 go test -tags "fts5,kit_posthog_disabled" ./internal/parser -run '^TestReconstructJSONL$' -count=1` | PASS; ordinary replay and empty-file behavior remain unchanged. | `file:///D:/Repos/.claude/pr-sweep/captures/agentsview-PR-TARGET-1412-review-6/ordinary.log` |
| provider-routing | VS Code Copilot JSONL and Positron provider controls | `CGO_ENABLED=1 go test -tags "fts5,kit_posthog_disabled" ./internal/parser -run 'TestParseVSCodeCopilotSession_JSONL\|TestPositronProviderParseSession' -count=1` | PASS; shared parser entrypoints and Positron identity remain intact. | `file:///D:/Repos/.claude/pr-sweep/captures/agentsview-PR-TARGET-1412-review-6/provider-routing.log` |
| limits | invalid-limit and default-wiring tests | `CGO_ENABLED=1 go test -tags "fts5,kit_posthog_disabled" ./internal/parser -run 'TestVSCodeCopilotReplayLimitsRejectInvalid\|TestVSCodeCopilotReplayDefaults' -count=1 -v` | PASS; non-positive and inverted limits report the specific validation errors before parser access, and defaults remain 64 MiB normal / 128 MiB hard. | `file:///D:/Repos/.claude/pr-sweep/captures/agentsview-PR-TARGET-1412-review-6/limits-wiring.log` |
| vet | parser static validation | `go vet -tags "fts5,kit_posthog_disabled" ./internal/parser` | PASS. | `file:///D:/Repos/.claude/pr-sweep/captures/agentsview-PR-TARGET-1412-review-6/vet.log` |
| diff | whitespace validation | `git diff --check` | PASS. | `file:///D:/Repos/.claude/pr-sweep/captures/agentsview-PR-TARGET-1412-review-6/diff-check.log` |

## Invariant Site Enumeration

The round-2 enumeration was regenerated against the final working tree with
the recorded threshold searches plus the non-fatal fixture-guard search. It
was asserted successfully, with all rows dispositioned and state-domain
coverage checked.

Enumeration artifact: `D:\Repos\.claude\pr-sweep\agentsview-PR-TARGET-1412-CHANGE-2.md`

The test-scale correction is verified by source inspection and assertions in
`internal/parser/vscode_copilot_test.go`: the shared test limits are 8/16 KiB,
the response builder takes a requested payload size, the exact-boundary record
is below 12 KiB, issue-shaped output is below 20 KiB, and kind-1/2 records are
below 12 KiB each. The size checks are fatal before file I/O. No threshold test
writes a production-scale record.
