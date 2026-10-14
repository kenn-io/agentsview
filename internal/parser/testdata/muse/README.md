# Muse Code fixture

`sessions/` is a synthetic Muse Code data tree. The content was written for
these tests; the record shapes follow the public evidence listed under Muse Code
in `docs/internal/session-format-sources.md`.

The parent log under `2026/09/10/` covers:

- a `retained_frame` whose child is a JSON-encoded envelope
- a `retained_marker` for an omitted live-only record
- a task `started` event that is not a user turn
- a response whose usage is logged before its tool calls
- a response whose usage is logged after its text
- a `model_completed` record with no open response; this shape is synthetic and
  has not appeared in a real Muse log
- a record mirrored from the subagent's stream
- automatic and user-chosen session names
- a partial last line, as a session still being written leaves it

The subagent log under `subagent/` has no workspace of its own. The
`approval-review/`, `tool-outputs/`, and `.msp-view-v1/` files must be ignored
by discovery.
