---
last_edited: 2026-09-21
---

# Agent-memory verification record

The live conversation-memory release gate passed 24 of 24 behavior runs on
commit `77bc9e0e64ca1a0df94662c42e64ef089b1b00ea`. The tested binary reported
the same commit and was built from a clean worktree. The run started at
2026-09-21T17:31:37Z and finished at 2026-09-21T17:44:09Z.

| Client      | Version | Configured / trace model              | Effort | Explicit | Implicit | Correction | Current context |
| ----------- | ------- | ------------------------------------- | ------ | -------: | -------: | ---------: | --------------: |
| Claude Code | 2.1.278 | `claude-sonnet-5` / `claude-sonnet-5` | medium |      3/3 |      3/3 |        3/3 |             3/3 |
| Codex CLI   | 0.155.1 | `gpt-5.6-luna` / not reported         | medium |      3/3 |      3/3 |        3/3 |             3/3 |

Every historical run called both `search_content` and `get_messages`, used the
expected decision or correction, and cited the source session and message range.
Every current-context run returned the supplied value without calling either
history tool. The fixture had no embedding index, so historical runs disclosed
the semantic-search failure and used the explicit lexical fallback.

| Client and case              | Latency minimum | Latency median | Latency maximum |
| ---------------------------- | --------------: | -------------: | --------------: |
| Claude Code, explicit        |           30.4s |          33.7s |           38.3s |
| Claude Code, implicit        |           35.1s |          37.2s |           49.7s |
| Claude Code, correction      |           37.5s |          42.8s |           46.8s |
| Claude Code, current context |            1.2s |           1.4s |            1.8s |
| Codex CLI, explicit          |           24.0s |          26.3s |           44.9s |
| Codex CLI, implicit          |           40.8s |          54.2s |           65.4s |
| Codex CLI, correction        |           32.2s |          38.0s |           39.3s |
| Codex CLI, current context   |            3.7s |           3.8s |            4.2s |

Claude Code reported 726 uncached input tokens, 1,172,071 cache-read tokens,
165,221 cache-write tokens, 31,510 output tokens, and $0.77231025 across the 12
behavior runs. Codex reported 1,301,187 input tokens, including 1,095,936 cached
tokens, and 10,426 output tokens. Codex CLI did not report cost in its JSONL
events, so no Codex dollar total is available.

The same synthetic fixture exposed several failures before the final run. Fresh
source sessions were excluded by active, one-shot, and automated defaults;
search snippets were sometimes treated as source reads; citations were omitted;
and proactive recall activation was inconsistent. The final adaptation includes
those session classes, requires `get_messages`, requires citations, and gives
existing-project planning a stronger skill trigger. These diagnostics were not a
controlled model-matched benchmark against the upstream implementation.

The ordinary SQLite tests, PostgreSQL package tests, generated-package tests,
documentation build, vet, golangci, and nil analysis passed locally. The full Go
suite reached its existing ten-minute timeout in the unchanged sync package's
1,030-session Omnigent fixture, both in the full run and when isolated. The
container-backed PostgreSQL target could not run because Docker was unavailable
on the test host.

This result covers native-client behavior against a synthetic local SQLite
archive. It does not measure semantic ranking, every model, or a private hosted
archive. The private-archive pilot remains separate, and its prompts, traces,
session identities, and findings must stay outside the repository. Raw artifacts
from this run remain in the ignored local test directory.
