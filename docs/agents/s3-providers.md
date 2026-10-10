# S3 Provider Rules

Read this file before enabling S3 ingest for an agent, changing `S3Provider` /
`Source.S3Discovery`, or altering S3 discovery, session IDs, temp
materialization, or post-fetch hydrate.

Operator configuration and object layouts live in `docs/configuration.md` under
S3-Compatible Session Sources. This guide is the implementer contract.

## When S3 Fits

S3 ingest is for single-file session objects under:

```text
s3://<bucket>/.../<machine>/raw/<agent>/...
```

The `<agent>` path segment must be the provider's `AgentType` string. The engine
uses that segment plus `Source.S3Discovery` to decide which objects belong to
which agent. Multi-file containers, SQLite stores, and agents that cannot name a
session from one object path do not belong here.

## Required Surface

1. Implement `parser.S3Provider` in `internal/parser/s3_provider.go`.
1. Set `Source.S3Discovery` to `CapabilitySupported` on both the factory and the
   constructed provider. Startup migration rejects the capability if the
   provider does not implement the interface.
1. Route `s3://` roots in `Discover` / `DiscoverEach` through
   `s3PrefixScan(root, scanner)` so provider discovery and engine dispatch use
   the same keep/project rules.
1. Skip `s3://` roots in `WatchPlan`. Object storage is not a local watch tree.

## Default Versus Custom

Embed `DefaultS3Provider` when the agent matches the common layout:

- Keep objects whose names end in the declared extensions.
- Project is the first path segment under the configured root.
- Session ID is `IDPrefix` plus the filename stem.
- Temp path strips `raw/<agent>`.
- No sidecar folding and no post-fetch hydrate.

Write a dedicated `<agent>_s3.go` when any of those rules is wrong. Current
custom implementations:

| Agent  | Why it cannot use the default alone                                                                                                                                                                                                                                                                                                                                                                                            |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Claude | Sidecar fold, subagent paths, transcript keep rules                                                                                                                                                                                                                                                                                                                                                                            |
| Codex  | `rollout-*.jsonl` keep, `codex:` UUID IDs, sessions/archived rewrite, parent and `session_index` locate. TraeX shares the type and must not enable S3.                                                                                                                                                                                                                                                                         |
| Cursor | Harvest `<project>/<id>.{jsonl,txt}` plus local `agent-transcripts` layouts, including a parent session's `subagents` directory; decode encoded project dirs only for the local layout; deduplicate a session stem across configured roots by source machine and raw project directory, preferring a session's own nested or flat object over a copy in another session's `subagents` directory, then `.jsonl`, then nested over flat, then lexical path |

`internal/parser/s3source.go` stays generic transport: client, list, fetch,
stat, and `s3PrefixScan`. Agent policy does not belong there.

## Identity Rules

`S3SessionID` is the session key before the engine prefixes the source machine.
Providers declaring `SharedSessionIDs` use the local collision policy for S3:
each distinct source keeps its saved ID, and a new source with an occupied ID
gets a derived ID. Cursor conversations in different projects keep their parser
relationships. An omitted remote object keeps its ownership
because discovery does not prove deletion. Codex does not yet declare this
capability.

- Keep only paths that are real transcripts for that agent.
- If `.jsonl` and `.txt` (or any other pair) share a source, keep one. Prefer the
  format the local parser prefers.
- Cursor selects current format and layout alternatives by machine, session
  stem, and raw project relative to the broadest configured root that accepts
  the object. Roots without a machine boundary use the local machine label.
- Archive ownership uses the exact object URI, with Cursor format and layout
  alternatives sharing the same machine, raw project, and session stem retaining
  their saved ID and stars. Alternatives under the same canonical root take
  precedence over other roots. Different projects keep separate conversations.
- Cursor subagents link to their project's archived parent, including when
  the child arrives first or the parent's object changes format. Refreshing a
  saved child after removing a source root keeps its verified same-family parent
  or unlinked state, including format changes. Wrong-project legacy links are
  repaired. Unlinked children prefer their own root's parent. Queued repairs
  wait while no Cursor S3 root is configured.
- Validate IDs with the same rules the local provider already uses.
- Do not invent a second discover wrapper that bypasses the scanner. Extra
  post-processing (prefer `.jsonl`, fold sidecars) belongs next to the
  scanner.

## Tests

Protect the keep and identity contract with concrete object URIs:

- A documented harvest object is discovered.
- A junk object with the same extension is not.
- Same-stem pairs produce one session ID and one discovered source.
- Forks that share a provider type stay off S3 unless they own the namespace.

## Sync Dispatch

After the capability is set, `internal/sync/s3.go` and
`internal/sync/s3_source.go` dispatch through `S3ProviderFor`. Do not add
another `switch file.Agent` for the new agent.
