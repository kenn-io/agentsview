---
last_edited: 2026-09-21
---

# AgentsView Memory

This native Claude Code and Codex package recalls relevant conversation history
through the focused AgentsView MCP profile. It bundles the same generated skill
used by `agentsview skills install`, plus Claude's bounded search agent and a
fail-open `SessionStart` hook for `startup`, `resume`, and `clear`.

Install the package with the client's native plugin flow and ensure `agentsview`
is on the client's `PATH`. The default target is the local archive owner. Select
one hosted role through the client's local environment:

| Role                          | Environment                                                                                                            |
| ----------------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| Local archive owner           | No variables required                                                                                                  |
| Hosted contributor            | `AGENTSVIEW_MEMORY_MODE=hosted-contributor`, `AGENTSVIEW_MEMORY_PG=true`, with optional `AGENTSVIEW_MEMORY_TARGET`     |
| Hosted reader over daemon     | `AGENTSVIEW_MEMORY_MODE=hosted-reader`, `AGENTSVIEW_MEMORY_SERVER`, and optional `AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE` |
| Hosted reader over PostgreSQL | `AGENTSVIEW_MEMORY_MODE=hosted-reader` and `AGENTSVIEW_MEMORY_PG=true`                                                 |

`AGENTSVIEW_DISABLE_AUTO_SYNC=1` disables the hook action while keeping existing
history searchable. Token values stay outside this package; the daemon target
uses a token-file reference or the existing `AGENTSVIEW_SERVER_TOKEN` runtime
environment.

PostgreSQL MCP reads use the target selected by `default_pg` in the AgentsView
configuration. For a named hosted contributor target, set `default_pg` to that
same target; `AGENTSVIEW_MEMORY_TARGET` selects the lifecycle owner but does not
retarget MCP reads.

Run `agentsview doctor memory --plugin-root <package-root>` to check the
selected archive and this package's skill, focused MCP configuration, and
SessionStart hook. The diagnostic reads metadata only and omits package paths,
server URLs, and tokens from its output.

The startup diagnostic reports a standalone AgentsView skill installed in the
same client home because loading both routes would duplicate the skill. It never
changes or removes that file. Remove the standalone copy manually after
reviewing any local edits. Native client uninstall owns only this package and
leaves the AgentsView archive intact.

The recall instructions and search agent adapt Episodic Memory at commit
`7e06519357777badd7a115d2014a7ef845904310`, Copyright (c) 2025 Jesse Vincent,
under the MIT License. The generated skill package includes the full notice in
`skills/agentsview-finding-history/LICENSE`.
