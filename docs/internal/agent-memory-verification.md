---
last_edited: 2026-09-21
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
and focused MCP profile to retrieve the evidence. The harness never scans the
operator's AgentsView archive and never copies authentication files.

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

This gate measures native-client behavior on a synthetic local archive. It does
not replace the PostgreSQL contract suite, prove behavior for every model, or
publish results from a private archive. A private hosted-archive pilot remains a
separate deployment check whose prompts, traces, and findings stay outside the
repository.

See the
[2026-09-21 verification record](2026-09-21-agent-memory-verification.md) for
the first measured release-gate result and its limits.
