---
title: One-shot CI capture
description: Capture exact usage from one supported non-interactive agent execution
---

# One-shot CI capture

`agentsview capture` is the automation-only path for measuring one exact agent
execution. It runs without the AgentsView daemon, web server, web interface,
watchers, or any listening socket. It does not replace the normal interactive
AgentsView experience.

The first schema version supports these direct producer commands:

- `claude -p` or `claude --print`;
- `codex exec --json`.

Other non-interactive producers need a future adapter that can prove exact
session identity and finality. AgentsView deliberately does not guess from a
process ID, the newest transcript, or a time-window scan.

On Windows, the producer command must resolve to a native executable.
AgentsView rejects `.cmd` and `.bat` shims because they cannot preserve an
untrusted argument boundary safely. The producer runs in its own console
process group; an interrupt is relayed to that group, and a failed relay or a
repeated interrupt terminates a producer that remains blocked.

Each supported command must start a fresh provider session. Claude continuation,
resume, fork, pull-request resume, and teleport options are rejected. Codex
`exec resume` is also rejected because its transcript includes usage from work
that happened before the captured execution.

Claude sources default to `${CLAUDE_CONFIG_DIR}/projects` or
`~/.claude/projects`; Codex sources default to `${CODEX_HOME}/sessions` or
`~/.codex/sessions`. Use `--provider-root` when the producer writes elsewhere.
The command reads only bounded custom-model pricing from AgentsView
`config.toml`; it does not resolve or open the normal AgentsView archive.

Every capture copies its exact provider JSONL into
`<capture-dir>/sources/` before accounting. AgentsView ingests only those
copies. This makes `capture report` independent of the original Claude or
Codex archive after the copy is complete.

`capture run` requires the final capture directory not to exist. It creates
that directory exclusively and secures it before writing state. The result file
must be outside the capture directory; use `--result -` with `capture report`
when standard output is preferred. Put capture state under a private workflow
directory or the runner's protected temporary directory. AgentsView rejects a
location whose parent chain allows another account to replace the capture
directory.

## GitHub Actions example

This job asks Claude Code to diagnose a generic build failure. The producer's
standard output and standard error remain ordinary live job output. The usage
result goes to a separate file and is uploaded with the standard artifact
action.

```yaml
name: diagnose-build

on:
  workflow_dispatch:

jobs:
  diagnose:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    env:
      AGENTSVIEW_OCCURRENCE: diagnose-${{ github.run_id }}-${{ github.run_attempt }}
      AGENTSVIEW_CAPTURE_DIR: ${{ runner.temp }}/agentsview-diagnose-${{ github.run_id }}-${{ github.run_attempt }}/capture
      AGENTSVIEW_USAGE_RESULT: ${{ runner.temp }}/agentsview-diagnose-${{ github.run_id }}-${{ github.run_attempt }}/usage.json

    steps:
      - uses: actions/checkout@v4

      - name: Install AgentsView
        run: curl -fsSL https://agentsview.io/install.sh | bash

      - name: Diagnose the build
        id: capture
        env:
          ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
        shell: bash
        run: |
          set +e
          agentsview capture run \
            --provider claude \
            --occurrence "${AGENTSVIEW_OCCURRENCE}" \
            --capture-dir "${AGENTSVIEW_CAPTURE_DIR}" \
            --result "${AGENTSVIEW_USAGE_RESULT}" \
            -- claude -p \
              "Inspect the failing build logs and explain the likely cause."
          status=$?

          echo "status=${status}" >> "${GITHUB_OUTPUT}"
          exit 0

      - name: Recover an interrupted report
        id: recover
        if: always()
        shell: bash
        run: |
          set +e
          status="${{ steps.capture.outputs.status }}"
          if [ -z "${status}" ]; then
            status=70
          fi
          if ! jq -e --arg occurrence "${AGENTSVIEW_OCCURRENCE}" \
            '.occurrence_id == $occurrence and
             .reporting.outcome == "complete"' \
            "${AGENTSVIEW_USAGE_RESULT}" >/dev/null 2>&1; then
            agentsview capture report \
              --capture-dir "${AGENTSVIEW_CAPTURE_DIR}" \
              --result "${AGENTSVIEW_USAGE_RESULT}"
            report_status=$?
            if [ "${report_status}" -eq 0 ] && \
              jq -e --arg occurrence "${AGENTSVIEW_OCCURRENCE}" \
                '.occurrence_id == $occurrence and
                 .reporting.outcome == "complete" and
                 (.execution.exit_code | type == "number")' \
                "${AGENTSVIEW_USAGE_RESULT}" >/dev/null; then
              status="$(jq -r '.execution.exit_code' \
                "${AGENTSVIEW_USAGE_RESULT}")"
            fi
          fi
          echo "status=${status}" >> "${GITHUB_OUTPUT}"

      - name: Upload usage result
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: agentsview-usage
          path: ${{ env.AGENTSVIEW_USAGE_RESULT }}
          if-no-files-found: error

      - name: Detect complete transcript evidence
        id: transcripts
        if: always()
        shell: bash
        run: |
          if [ -s "${AGENTSVIEW_CAPTURE_DIR}/sources/bundle.json" ]; then
            echo "available=true" >> "${GITHUB_OUTPUT}"
          fi

      # This artifact contains prompts, responses, and tool output. Restrict
      # access and retention independently from the usage JSON above.
      - name: Upload transcript evidence
        if: always() && steps.transcripts.outputs.available == 'true'
        uses: actions/upload-artifact@v4
        with:
          name: agentsview-transcripts
          path: ${{ env.AGENTSVIEW_CAPTURE_DIR }}/sources
          if-no-files-found: warn
          retention-days: 1

      - name: Preserve the producer outcome
        if: always()
        shell: bash
        run: exit "${{ steps.recover.outputs.status }}"
```

Use the install method and version pin required by your organization. The
occurrence-specific paths prevent another workflow attempt from supplying stale
state. Recovery changes the saved status only after it successfully replaces a
missing or failed report; a complete initial report therefore cannot hide a
post-start stream error. The example uploads usage and transcripts as separate
artifacts because they have different disclosure and retention needs. Removing
the capture directory after successful export deletes its source copies and
recovery database from the runner. Do not remove it before the final
`capture report` attempt.

`capture run` does not impose a runtime timeout on Claude or Codex. Use the
workflow's `timeout-minutes` for that policy. AgentsView applies only its
bounded post-exit finalization timeout. That deadline covers source polling,
copying, isolated database setup, ingestion, usage-row sorting and
deduplication, pricing, token projection, shutdown, and verification. If these
flags are customized, `--quiescence` must be shorter than
`--finalization-timeout`; invalid timing bounds are rejected before the producer
starts and cannot become recovery state.

## Codex

Codex must be in JSON mode so its `thread.started.thread_id` is a supported
machine contract:

```bash
agentsview capture run \
  --provider codex \
  --occurrence "${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}-${GITHUB_JOB}" \
  --capture-dir "${RUNNER_TEMP}/agentsview-codex-capture" \
  --result "${RUNNER_TEMP}/agentsview-codex-usage.json" \
  -- codex exec --json "Diagnose the build failure."
```

The JSONL stream is copied byte-for-byte to standard output. AgentsView does
not parse formatted stderr and does not add `--json` itself. Schema version 1
reports Codex cache-write and reasoning-output categories as unavailable, not
zero, because the canonical parser does not yet retain them. Every Codex v1
result therefore has `assurance.state: partial`; this is expected, not a
reporting failure.

The initial adapter requires `exec` immediately after the resolved executable.
For example, use `codex exec -c key=value --json ...`; a global-option form such
as `codex -c key=value exec --json ...` is unsupported in schema version 1.
`codex exec resume` is not a one-shot capture input.

## Claude wrappers and standard input

For a direct `claude` executable, AgentsView creates a UUID and passes
`--session-id` to the child. It also accepts a UUID already present in the
Claude arguments.

If an intervening script is required, make it pass a caller-chosen UUID to
Claude, then supply the same UUID with `--session-id` to `capture run`:

```bash
session_id="$(uuidgen | tr '[:upper:]' '[:lower:]')"
claude_work_dir="/path/to/claude/worktree"
agentsview capture run \
  --provider claude \
  --occurrence build-42 \
  --session-id "${session_id}" \
  --claude-work-dir "${claude_work_dir}" \
  --capture-dir ./capture-build-42 \
  --result ./usage-build-42.json \
  -- ./run-claude-ci.sh "${session_id}"
```

The script must invoke non-interactive Claude with that exact `--session-id`.
`--claude-work-dir` is required for wrappers and must name the directory from
which the script actually starts Claude, after any directory changes. AgentsView
cannot verify arguments hidden inside the script; a wrong UUID or working
directory ends as `no_session` rather than selecting a nearby transcript. The
script must start a fresh session; it must not continue, resume, or fork prior
work.

The UUID must use lowercase hexadecimal and be new: `capture run` rejects
uppercase UUIDs, an existing exact root transcript, or a delegated-session tree
before it starts the producer. AgentsView also reserves the provider root and
UUID through finalization, so concurrent captures cannot claim the same
deterministic transcript.
The provider root retains one owner-private reservation registry lock; active
per-session entries are removed when finalization ends and contain no transcript
content.

Standard input is inherited unchanged, so prompt-on-stdin works:

```bash
printf '%s\n' 'Diagnose the build failure.' | \
  agentsview capture run \
    --provider claude \
    --occurrence build-42 \
    --capture-dir ./capture-build-42 \
    --result ./usage-build-42.json \
    -- claude -p
```

## Outcomes and retries

Child execution and reporting are separate in the result:

- If reporting succeeds, `capture run` exits with the child status.
- A failed child can still have complete or partial usage.
- If the child succeeds but reporting fails, the command exits with status
  `70`.
- If both fail, the command preserves the child's status and records the
  reporting failure in the result.
- A standard-stream error after the child starts does not become
  `child_start_failed`. AgentsView preserves the child outcome, attempts usage
  finalization, and surfaces the stream error from the command.
- `SIGINT` and `SIGTERM` are forwarded to the child. AgentsView then records
  the signal and attempts the bounded finalization pass.

Once the wrapper starts, it writes a result even when reporting fails. A
missing result means the wrapper itself stopped before it could report. Retry
from the same private capture directory without rerunning the producer. This
also covers a signal that arrives after the child exits but before the bounded
finalization pass writes the result. Once `sources/bundle.json` is complete,
recovery does not need the producer's home directory or live archive:

```bash
agentsview capture report \
  --capture-dir ./capture-build-42 \
  --result ./usage-build-42.json
```

`capture run` removes any existing result file before it starts the producer.
Consumers must still verify that `occurrence_id` matches the requested
occurrence before accepting or uploading a result.

A completed capture is sealed. Replaying it emits the same bytes. Failed
recovery attempts emit and preserve the first failure envelope; only a
reporting-complete retry replaces and seals it. Running a new
producer in a directory that already contains an occurrence is an explicit
conflict; use `capture report` instead. The caller-supplied occurrence ID is
opaque correlation data, not a person, organization, or ownership label.

## Failure reference

The result's `reporting.reason` is a closed value:

| Reason | Meaning | Action |
| --- | --- | --- |
| `no_session` | The exact provider session never appeared | Check producer persistence and the provider root |
| `multiple_sessions` | More than one file claimed the exact Codex thread ID | Preserve the capture directory and investigate the conflicting files |
| `finalization_timeout` | The bounded polling, copy, ingestion, and verification pass did not finish before the deadline | Retry `capture report` or increase `--finalization-timeout` on the original run |
| `child_start_failed` | The producer executable could not start | Correct the executable, permissions, or working directory; do not treat it as a child exit |
| `correlation_unavailable` | Codex did not emit one bounded, valid `thread.started` ID | Require a supported `codex exec --json` build and preserve its output for diagnosis |
| `correlation_conflict` | Codex emitted conflicting thread IDs | Preserve the capture directory and producer output; do not choose either session |
| `source_limit` | The exact delegated source tree exceeded its file or entry bound | Preserve the directory and reduce the producer fan-out |
| `source_bytes_limit` | A source, JSONL line, or aggregate exceeded its byte bound | Preserve the directory and inspect producer output size |
| `source_unavailable` | An exact source disappeared, persisted evidence no longer matches, or a Claude child reference has no captured transcript | Preserve the capture directory; restore the missing source if possible and retry `capture report` |
| `ingest_failed` | Exact-path parsing or isolated SQLite accounting failed | Keep the capture directory and report the producer and AgentsView versions |

Failure envelopes never contain transcript text, prompts, responses, source
paths, environment values, or credentials. The capture directory contains the
provider JSONL, parsed message content in `capture.db`, and local source paths
in its private manifest. Treat the entire directory as sensitive diagnostic
state. AgentsView uses owner-only mode bits on Unix and a protected Windows
access-control list limited to the current user, Local System, and local
administrators. It also rejects replaceable parent directories and creates the
isolated SQLite archive with owner-only permissions before opening it.

The `sources/` subdirectory is independently uploadable. It contains the
provider-relative JSONL files and `bundle.json`, whose closed schema is
`agentsview.one-shot-transcripts` version `1`. Each bundle entry associates a
provider session ID with an `application/jsonl` raw-source reference containing
a relative path, SHA-256 hash, and byte count. Claude subagent IDs use the
provider's `agent-...` identity from the transcript filename; they are not
UUIDs. Every captured Claude subagent contributes to usage even when an
interrupted producer did not flush its parent-link record. Consumers must
reject unknown bundle schema names, versions, fields,
media types, unsafe paths, or mismatched hashes. The bundle contains transcript
content and is not safe to publish with the usage JSON. AgentsView keeps
`sources/` after sealing and never removes it automatically.

To import an extracted bundle into the normal AgentsView archive later, point
the matching provider directory setting at the provider root inside `sources/`
and run the ordinary sync command. Do not point it at `sources/` itself:

```toml
# Use the entry that matches the bundle's provider.
claude_project_dirs = ["/path/to/sources/claude/projects"]
codex_sessions_dirs = ["/path/to/sources/codex/sessions"]
```

Then run `agentsview sync`. This optional import uses the normal AgentsView
experience; one-shot capture and `capture report` do not require it or start
its services.

Preflight errors happen before the wrapper starts and therefore do not create a
result. These include a missing or oversized occurrence ID, an unsupported
provider invocation or continuation mode, an invalid or previously used
explicit UUID, a result path inside capture state, and any pre-existing capture
directory. The result path must also be outside the provider root. The provider
root cannot be the capture directory or a child of it. Correct a malformed
invocation. For an existing occurrence, use `capture report`; never delete its
state merely to turn a retry into a second occurrence.
Recovery requires a current-user-owned directory with a valid manifest and only
capture-owned top-level entries before AgentsView changes or cleans state.

## Result contract

The JSON envelope has schema name `agentsview.one-shot-usage` and version `1`.
Consumers must reject unknown schema names, versions, and closed enum values.
A present token field with value zero is proven zero. An omitted field is
unavailable. Money uses AgentsView's exact integer-microdollar object and a
separate `USD` currency; no floating-point monetary field is emitted.

The envelope includes exact root and delegated provider session IDs,
provider-derived timestamps when available, child exit or signal outcome,
canonical token categories, canonical cost provenance, bounded model and
producer metadata, path-free source SHA-256 and byte provenance, and an
assurance state. It contains no usage breakdown or transcript-derived content.
The `sources` array links each persisted provider source to the sensitive
bundle without revealing its provider-relative path.

`assurance.state` is `complete` when every category AgentsView promises for the
adapter is proven, `partial` when useful facts are present with a named gap, and
`unavailable` when usage cannot be proven. Assurance reasons include
`unfinished_session`, `malformed_transcript`, `cost_unavailable`,
`usage_unavailable`,
`codex_cache_write_unavailable`, `reasoning_output_unavailable`,
`unpriced_model`, and `metadata_truncated`. These are not reporting failures:
consumers may use the proven fields while preserving the stated limitation.

A quiescent transcript can be `unfinished_session` after the producer exits,
for example when CI interrupts a pending tool call. AgentsView seals the usage
it can prove instead of waiting for a dead writer. `cost_unavailable` means no
authoritative complete cost exists; omitted cost is never silently treated as
zero. `malformed_transcript` means at least one source line could not be parsed,
so the visible usage may undercount the producer's total. When stored output
includes messages not represented by canonical usage
rows, AgentsView reports that output but omits input and cache fields and adds
`usage_unavailable` so categories with different provenance are not mixed.
