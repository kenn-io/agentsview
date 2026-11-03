package main

// rootAgentGuide supplies the workflow and scripting contract before the menu.
const rootAgentGuide = `Agent quick start:
  1. Discover supported agents and their default source locations:
     agentsview source list --json
  2. Refresh the local archive (the background daemon also watches sources):
     agentsview source sync
  3. Find recent sessions and copy a full ID from the result:
     agentsview session list --since 7d --limit 20 --json
  4. Inspect metadata, then a bounded transcript window:
     agentsview session get <id> --json
     agentsview session messages <id> --around 40 --before 5 --after 5 --json
  Search and reports:
     agentsview session search 'connection reset' --since 14d --json
     agentsview session usage <id> --json
     agentsview report stats --since 28d --json
     agentsview report usage daily --since 28d --json

Session IDs:
  Treat IDs as opaque strings.
  Claude IDs are commonly bare UUIDs; other agents use forms such as codex:<id>.
  Remote-synced IDs may be host~codex:<id>. Several read commands retry bare
  provider IDs with known agent prefixes; a full ID avoids choosing a different
  provider's session. Metadata IDs, transcript ordinals, and pagination cursors
  are different values. Use the corresponding response cursor. For session
  list, repeat the backend, filters, and sort on every page; its cursor does
  not store the filter selection.

Where data comes from:
  SQLite under the data directory is the persistent local archive. Normal local
  reads use a discovered daemon, starting one in the background when permitted.
  Discovery prefers a writable daemon; if only a compatible read-only mirror
  server is available, supported reads can use that server.
  On the session group, --server URL selects a remote daemon and --pg selects
  configured PostgreSQL; these options are mutually exclusive. Remote auth uses
  --server-token-file or AGENTSVIEW_SERVER_TOKEN. These are command/group flags,
  not root flags. Read-only mirrors cannot refresh local provider sources.
  AGENTSVIEW_NO_DAEMON=1 prevents automatic startup and replacement. Ordinary
  local reads then need a running daemon; explicit offline and maintenance
  commands retain their documented local paths. Manage the writable daemon with:
     agentsview daemon status
     agentsview daemon start
  source list reports built-in defaults and environment override names, not
  configured paths.

Output:
  Human output is the default. Where offered, --json aliases --format json.
  Successful reports go to stdout; diagnostics, warnings, and errors go to
  stderr. Progress is command-specific: source sync human progress also uses stdout;
  most other progress uses stderr.
  Human failures use fatal: followed by hint: on stderr. JSON-selected failures
  use a flat stderr document: {"error":"validation","message":"...","hint":"..."}.
  JSON selection follows each command's flags; there is no root --json flag.
  session watch emits NDJSON events; session export emits provider-specific
  source content (Markdown, JSON, JSONL, or file bytes).
  Their --format/--json flags are rejected.

Exit codes:
   0  Success. usage-batch keeps each ID's status in its result document.
   1  General failure; ordinary session lookups also use this when not found.
   2  session usage/token-use: session not found; source raw backfill: failed batch.
   3  session usage/token-use: report has no token, cost, or plan figures;
      data-version check: archive requires a newer binary.
   4  Export cursor reset or reconciliation required.
   5  Conversation revision changed during export.
  64  Invalid command usage, such as bad flags, arguments, or flag conflicts.
  65  Invalid values, including malformed time windows and sort selections.
  69  A required service or capability is unavailable.
  70  Capture report failure. capture run preserves the producer's exit code.
  No-data 3 is silent after its report. Explicit command and child-process
  codes take precedence.

Choose a command below, then use agentsview <command> --help for flags, limits, and examples.`
