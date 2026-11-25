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
per client. Pass `--client`, `--runs`, or `--case` through
`MEMORY_E2E_ARGS` to narrow a run.

The corpus and questions live in `scripts/memory-e2e/testdata/lookups/`.
They cover topic, named-session, ownership, recent-work, and long-result
lookups. Fixture transcripts appear under the client's `~/.codex/sessions/`
path, outside its working directory. Expected IDs and answer phrases stay
outside the project visible to the agent. Codex disables `mcp_2026_07_28` so the capture records each MCP call under a
fixed protocol. Opaque exec scripts without individual results stay unscored.

The report counts lookup calls through the first result that supplies every
expected answer value for the cited session. A result counts when its text names
the session, or when its non-empty, error-free call input names it. A result that
names several sessions counts for each one. Facts may arrive across multiple
results. The count includes MCP, CLI, direct reads, saved-output reads, and
AgentsView tool discovery.

Each question asks for a JSON object with named answer fields and the exact
source session ID. The scorer checks every expected value, so a wrong choice,
contradiction, or changed identifier spelling cannot pass on keyword overlap.
Errors, empty results, truncation, and calls per route appear per attempt.
Wrong answers, timeouts, client failures, and unsupported answers in a complete
capture fail. An otherwise correct answer stays unscored when capture is
incomplete. If no hit is visible and calls are missing or untimed, the result
also stays unscored. One- and two-call shares use all scheduled attempts.
Client setup errors stop the run.

The client receives temporary copies of its auth files for the run; the
harness removes those copies after each attempt. Lookup captures are private;
remove them when they are no longer needed.

This gate measures native-client behavior on a synthetic local archive. It does
not replace the PostgreSQL contract suite, prove behavior for every model, or
publish results from a private archive. A private hosted-archive pilot remains a
separate deployment check whose prompts, traces, and findings stay outside the
repository.

See the
[2026-09-21 verification record](2026-09-21-agent-memory-verification.md) for
the first measured release-gate result and its limits.
