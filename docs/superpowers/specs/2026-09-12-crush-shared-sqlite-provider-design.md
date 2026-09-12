# Crush shared SQLite provider design

## Goal

Keep Crush session sync correct and efficient without maintaining a private copy
of the repository's SQLite discovery, observation, and freshness logic.

## Scope

This change keeps Crush on the shared DB-backed provider used for SQLite files
that contain many logical sessions. It consolidates the row-change observer that
Crush currently duplicates from Goose and reuses the existing SQLite
container-state checks for database and write-ahead-log changes.

Crush-specific code remains responsible only for:

- expanding `projects.json` entries into Crush data directories;
- mapping each data directory back to its configured root and project path;
- reading the Crush session, message, and usage schema;
- hashing the Crush fields that determine one session's parsed result; and
- decoding Fantasy message parts into AgentsView messages and tool events.

The separate Goose and OpenCode workspaces own any provider migrations outside
this pull request.

## Shared SQLite design

`dbBackedProvider` remains the provider integration because it already owns
virtual session paths, streamed discovery, watch plans, stored-source lookup,
reconciliation, parsing, and raw SQLite capture for this source shape.

Move the provider-neutral row observation code out of `crush_provider.go` into a
shared SQLite helper. The helper tracks SQLite container identity and one
monotonic cursor per configured table. A provider supplies the table name, the
cursor expression, the session-ID expression, and the row-identity expression
needed to map new rows to logical sessions. The helper supplies locking,
cold-start behavior, database-replacement detection, cursor validation, and
ordered deduplication.

Use `SQLiteContainerState` for physical database identity and SQLite change
markers. Do not repeat file identity, write-ahead-log, or replacement logic in
the Crush provider. Keep schema-version checks in the provider callback because
schema compatibility belongs to the producer format.

Crush configures the shared observer for the `sessions` and `messages` tables.
It continues to use a full per-session content fingerprint during full sync and
reconciliation, so unchanged sessions skip parsing and same-second message
changes remain visible.

## Archive behavior

AgentsView remains a persistent archive. Removing a session row from `crush.db`,
removing a project from `projects.json`, or removing the source database does
not delete the archived session or mark it missing solely because the producer
no longer lists it. Session deletion remains an explicit AgentsView action.

Crush declares this through a provider capability so reconciliation and direct
parse paths apply the same policy without hard-coding Crush in the sync engine.

Root normalization must still accept a Crush registry directory, a project data
directory, or a direct `crush.db` path. Reconciliation may use those spellings
to find and refresh sessions that still exist, but it must not treat an omitted
Crush row as deletion authority.

## Message decoding

Keep the small Crush Fantasy-part decoder provider-specific. The shared JSON
content helper expects flat content blocks, while Crush persists each part's
fields below `data`. Expanding the shared JSON helper for one incompatible shape
would add branching without reducing the SQLite integration code that caused the
current sprawl.

## Verification

Behavioral tests must prove that:

- a full sync skips an unchanged Crush session;
- a write affecting one session does not reparse unchanged sibling sessions;
- same-second message inserts and registry project-path changes refresh the
  affected session;
- all supported Crush root spellings discover the same sessions;
- removing a Crush session or source leaves its archived AgentsView session
  intact; and
- the shared observer handles cold start, concurrent cursor publication, and
  SQLite database replacement independently of Crush.

Run the focused parser and sync tests, followed by `go fmt ./...`,
`go vet ./...`, and the repository's normal commit hooks.
