# Skills Pack Design

Status: approved design, implemented in the semantic-search PR (#999). This is a
working document; at the end of the skills work its durable content folds into
the user docs and this file is deleted, matching the repo's spec lifecycle.

## Goal

Ship agent-facing workflow knowledge with agentsview so coding agents (Claude
Code, Codex, and anything honoring the `.agents/skills` convention) reliably use
the CLI/MCP surface to find prior sessions and reconstruct design decisions and
intent — instead of improvising CLI calls.

## Scope

One skill: `agentsview-finding-history`. A `skills` CLI command group that
renders it from an embedded template and installs it into harness discovery
paths. One search-ergonomics addition: a relative `--since` filter.

Non-goals: a Claude Code plugin/marketplace, additional skills (analytics,
usage), uninstall/update subcommands (update = install), MCP prompt delivery.

## Generation model

- Canonical source: `internal/skills/templates/finding-history.md.tmpl`,
  embedded into the binary via `go:embed` (same pattern as `internal/web`);
  the `internal/skills` package owns the renderer and harness table. No
  rendered copies are checked in. (Not top-level `skills/` — the repo's own
  agent-development skills already live in `.agents/skills/` and the two must
  not be confused.)
- Rendering: Go `text/template`. Template data is a harness descriptor (idiom
  phrases, frontmatter dialect) — content is otherwise identical across
  harnesses.
- The rendered file begins with a generated-by header comment containing: the
  agentsview version string AND a sha256 content hash of the rendered body
  (hash excludes the header line itself, so it is reproducible).

## Harness table

| `--harness` | user-level target                                          | project-level target |
| ----------- | ---------------------------------------------------------- | -------------------- |
| `claude`    | `~/.claude/skills/agentsview-finding-history/SKILL.md`     | `.claude/skills/...` |
| `agents`    | `$HOME/.agents/skills/agentsview-finding-history/SKILL.md` | `.agents/skills/...` |

`agents` is the open cross-agent convention (Codex reads it, per
developers.openai.com/codex/skills). There is deliberately no `codex` harness.
Default: install both harnesses, user level. `--project` switches to
project-level paths (relative to the current working directory's repo root when
inside a git repo, else CWD).

## `skills` command group

- `agentsview skills install [--harness claude|agents]... [--project] [--force]`
  Renders per harness and writes. Overwrite rules:
    - Target missing: write.
    - Target exists and its actual content hash matches the hash recorded in its
      own header (unmodified generated file): overwrite.
    - Otherwise (user-modified, or no header — a hand-authored file): refuse with
      a clear message; `--force` overrides.
- `agentsview skills list` — for each harness/level, report installed / missing
  / stale (recorded hash differs from freshly-rendered hash) / modified
  (content differs from recorded hash).
- Version comparison is never used for staleness (dev builds report version
  "dev"); the content hash is authoritative.

## `--since` filter

`session search --since <value>` and `session list --since <value>`:

- Relative: `Nh`, `Nd`, `Nw`, `Nm` (months), `Ny` — e.g. `--since 3m`,
  `--since 14d`. Computed against the current time, mapped to the existing
  `active_since` filter (RFC3339), so "recent" means "active recently"
  including long-running sessions.
- Absolute: `YYYY-MM-DD` accepted and converted to that date's midnight local
  time.
- Mutually exclusive with `--active-since` (error, mirroring existing
  mutually-exclusive flag errors). Pure CLI sugar: no HTTP/service/store
  changes; the flag resolves to the existing `active_since` parameter.

## Skill content

Source of the skeleton: the existing `finding-agentsview-history` draft (bounded
subagent dispatch, mechanical budgets, rationalization table, evidence-first
output with `session-id` + ordinal citations). Revisions:

- Concept queries lead with `session search --hybrid` (semantic + FTS fused);
  exact identifiers (paths, error strings, tool names) use plain substring/FTS
  probes.
- Fallback discipline: if semantic/hybrid returns the "semantic search not
  available" error (`ErrSemanticUnavailable` family / HTTP 501), fall back to
  the FTS probe strategy. If it returns the transient endpoint error (503 /
  "temporarily unavailable"), report the outage and retry once — never
  silently downgrade, so a broken embeddings setup stays visible.
- Triage with `--context 2` inline windows; deep-dive with
  `session messages <id> --around <ordinal> --role user,assistant`.
- Recency narrowing with `--since` (e.g. `--since 3m` for recent decisions).
- Decision-archaeology recipe: locate the earliest substantive mention (semantic
  query + `--since` widening), then walk forward — user messages carry intent,
  assistant messages carry rationale and tradeoffs; watch tool activity for
  spec/plan artifacts; produce a decision record citing session IDs and
  ordinals.
- Frontmatter description triggers on: "why did we decide…", "how did we do this
  before", "find the session where…", recovering prior
  instructions/examples/decisions from recorded agent history.

## Docs

`docs/semantic-search.md` gains a short "Skills for coding agents" section: what
`skills install` does, where files land, when to re-run it. The commands
reference gets an `agentsview skills` entry.

## Testing

- Renderer: golden-free behavioral tests — rendered output parses as a skill
  file (frontmatter present, name matches directory), harness idiom
  substitution applied, hash in header matches body hash.
- Install: table-driven over (missing / unmodified / modified / headerless) ×
  (--force on/off) using t.TempDir as fake home; `skills list` states.
- `--since`: parser table (units, absolute date, rejects garbage, month/year
  arithmetic), flag exclusivity error, and threading into `active_since` via
  the existing fake-service command tests.
