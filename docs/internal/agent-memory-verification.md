---
last_edited: 2026-11-24
---

# Conversation-memory verification

AgentsView verifies conversation recall in three layers. The ordinary Go suites
cover the shared SQLite and PostgreSQL search/read contracts, revision-bound
evidence, filters, multi-concept retrieval, target selection, lifecycle
behavior, and generated native package contents. The plugin tests render the
Claude Code and Codex artifacts into temporary directories and verify ownership
and upgrade rules without reading a user's client home.

The final layer is an opt-in live gate. It creates an isolated project and
AgentsView data directory for each client, asks that client to record synthetic
decisions in one session, syncs only that transcript, and starts a no-sync
AgentsView server over loopback. Fresh client sessions then use the native skill
and focused MCP profile to retrieve the evidence. This memory suite does not
scan the operator's AgentsView archive or copy authentication files.

Run the complete release gate with authenticated Claude Code and Codex CLIs:

```bash
make memory-e2e
```

The default uses `claude-sonnet-5` for Claude Code and `gpt-5.6-luna` for Codex,
both at medium reasoning effort. It runs the explicit decision, implicit
failure, later correction, and current-context exception three times per client.
Every historical run must call both `search_content` and `get_messages`, use the
expected evidence, and cite a resolvable session and message range. The
current-context case must answer without either history tool. All three attempts
must pass.

For a bounded diagnostic run, pass arguments through the Make variable:

```bash
make memory-e2e MEMORY_E2E_ARGS='--client codex --runs 1 --case explicit-decision'
```

The command consumes the caller's configured model quota. Each agent invocation
has a five-minute default timeout. Raw source and behavior JSONL, stderr, the
isolated database, and `aggregate-result.json` are written beneath an ignored
`.test-data-memory-e2e-<timestamp>` directory. Keep those artifacts local
because agent traces may contain environment context. A public verification
record must contain only revisions, versions, configured models, trace-reported
models when available, aggregate outcomes, latency, token totals, and known
limitations.

## Lookup benchmark

Run `make memory-e2e MEMORY_E2E_ARGS='--suite lookups'` to measure common
questions against a synthetic archive. The benchmark needs Linux and
`bwrap`. It gives each client a private home and access to the fixture
archive, including its transcript files, while hiding the operator's home.
The client uses the full MCP profile and can use shell and file reads.
The run consumes model quota. By default, it runs three attempts per question
and one preflight per client. Pass `--client`, `--runs`, or `--case` through
`MEMORY_E2E_ARGS` to narrow a run.

The default corpus and questions live in `scripts/memory-e2e/testdata/lookups/`.
They cover topic, named-session, ownership, recent-work, child-session,
tool-result, message, and long-result lookups. Expected IDs and answer phrases
stay outside the project visible to the agent.
Pass `--lookup-manifest /path/to/manifest.json` to run a local corpus. The
manifest has `fixtures`, `sources`, and `questions` arrays. A source entry names
its `provider` (`claude` or `codex`), a transcript `path`, and an optional
`session_id` to check after sync. Relative source paths resolve beside the
manifest. The runner copies only those transcripts into each isolated project
before syncing them; it leaves the input files untouched. Keep manifests,
transcripts, prompts, and results private. The selected session content goes to
the configured model during a live run.
Each client first runs a search and message-read preflight. A client whose
capture cannot identify both results is marked unsupported and its scheduled
attempts remain unscored. Codex disables `mcp_2026_07_28` so the capture records
each MCP call under a fixed protocol. The selected Codex model uses code-mode-only
tool delivery, so the capture scores nested MCP calls when it records their own
results and timing. Opaque exec scripts without individual results stay
unscored. The report records the client's tool delivery mode.

The report counts lookup calls started by the time the first result containing
the cited item completed. It includes MCP, CLI, direct reads, saved-output
reads, and tool discovery. A hit must link the cited session to a returned
result, through its content or the paired call's arguments, and contain every
expected answer phrase. Message hits also require the cited ordinal in the
result. Direct transcript reads count as fallback calls and follow the same
rules.
Errors, empty results, truncation, route totals, and the route that supplied
the hit appear per attempt, alongside the captured source and missing-call
counts. Wrong answers and timeouts fail. Missing results
and calls whose timing cannot be compared leave a correct answer unscored.
The one- and two-call shares use all scheduled attempts; p50 and p90 use
scored successes. The report shows both per-question and overall figures.

The lookup suite stores private captures and reports under
`$XDG_CACHE_HOME/agentsview/memory-e2e/`, or the platform cache directory when
`XDG_CACHE_HOME` is unset. The client receives temporary copies of its auth
files for the run; the harness removes those copies after each attempt. Keep
the artifacts private and remove them when they are no longer needed.

This gate measures native-client behavior on a synthetic local archive. It does
not replace the PostgreSQL contract suite, prove behavior for every model, or
publish results from a private archive. A private hosted-archive pilot remains a
separate deployment check whose prompts, traces, and findings stay outside the
repository.

See the
[2026-09-21 verification record](2026-09-21-agent-memory-verification.md) for
the first measured release-gate result and its limits.
