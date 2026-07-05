# Skills Pack Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development to implement this plan task-by-task.
> Invoke the roborev-fix skill once after Task 4.

**Goal:** Ship the `agentsview-finding-history` agent skill, generated from an
embedded template by `agentsview skills install`, plus a relative `--since`
search filter.

**Architecture:** One canonical template in `internal/skills/templates/`,
rendered per harness (claude, agents) with a hash-carrying generated-by header;
a cobra `skills` command group installs into user/project discovery paths;
`--since` is CLI sugar resolving to the existing `active_since` filter.

**Tech Stack:** Go text/template, go:embed, cobra, testify.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-05-skills-pack-design.md` — its
  overwrite rules, harness table, and fallback discipline are binding.
- Build/test with `CGO_ENABLED=1` and `-tags fts5`; testify require/assert;
  `t.TempDir()`; never `if got != want { t.Fatalf }`.
- After Go changes run `go fmt ./...` and `go vet ./...`; commit every task;
  never amend; hooks must pass (mdformat runs on docs).
- Skill install must never write outside the harness target directories and
  never delete anything.

______________________________________________________________________

### Task 1: `--since` relative time filter

**Files:**

- Modify: `internal/timeutil/timeutil.go`, `cmd/agentsview/session_search.go`,
  `cmd/agentsview/session_list.go`
- Test: `internal/timeutil/timeutil_test.go`,
  `cmd/agentsview/session_search_test.go`,
  `cmd/agentsview/session_list_test.go`

**Interfaces:**

- Produces in timeutil:

    ```go
    // ParseSince resolves a --since value against now: relative forms
    // "Nh", "Nd", "Nw", "Nm" (months), "Ny", or an absolute YYYY-MM-DD
    // (that date's midnight in now's location). N is a positive integer.
    func ParseSince(now time.Time, s string) (time.Time, error)
    ```

    Months/years use `now.AddDate(-y, -m, 0)`; hours/days/weeks use `now.Add(-d)`.
    Errors name the accepted forms.

- Both commands gain
  `flags.StringVar(&since, "since", "", "Only sessions active since a relative duration (3m, 14d, 12h, 1y) or YYYY-MM-DD")`.
  When set: `--active-since` also set → error
  `"--since and --active-since are mutually exclusive"`; otherwise resolve via
  ParseSince(time.Now(), since) and assign the RFC3339 string to the existing
  activeSince variable. No service/HTTP/store changes.

**Steps:**

- [ ] Write failing parser table test (each unit, absolute date, garbage like
  "3x"/"m3"/""/"-3d" rejected, month arithmetic across year boundary).
- [ ] Implement ParseSince; PASS.
- [ ] Write failing command tests: `--since 14d` threads an RFC3339 active_since
  into the fake service for both commands; exclusivity error.
- [ ] Implement flag wiring; PASS. Run package tests.
- [ ] Commit `feat(cli): relative --since filter for session search and list`.

______________________________________________________________________

### Task 2: `internal/skills` package (template + renderer + harness table)

**Files:**

- Create: `internal/skills/skills.go`,
  `internal/skills/templates/finding-history.md.tmpl`
- Test: `internal/skills/skills_test.go`

**Interfaces:**

```go
package skills

// Harness identifies a skill discovery convention.
type Harness string

const (
    HarnessClaude Harness = "claude" // ~/.claude/skills
    HarnessAgents Harness = "agents" // ~/.agents/skills (Codex et al.)
)

func AllHarnesses() []Harness

// Rendered is one skill file ready to install.
type Rendered struct {
    Name    string // "agentsview-finding-history"
    Content string // full file: generated-by header + frontmatter + body
    Hash    string // sha256 hex of Content minus the header line
}

// Render produces the skill for a harness. version is the CLI version
// string, recorded in the header for humans (hash is authoritative).
func Render(h Harness, version string) (Rendered, error)

// TargetDir returns the directory the skill installs into for a harness:
// <base>/<claude-or-agents path>/agentsview-finding-history. base is the
// home dir for user-level installs or the project root for --project.
func TargetDir(h Harness, base string) string

// InstalledState classifies an existing file against a fresh render.
type InstalledState int

const (
    StateMissing InstalledState = iota
    StateCurrent          // content == fresh render
    StateStale            // unmodified generated file, but older render
    StateModified         // content no longer matches its recorded hash
    StateForeign          // no generated-by header
)

func Classify(existing []byte, fresh Rendered) InstalledState
```

Header format (first line of the file, above frontmatter — YAML comment so
frontmatter parsers still work):

```text
# generated-by: agentsview <version> hash:<sha256hex> — do not edit; re-run `agentsview skills install`
```

`Classify`: no header → StateForeign; recorded hash ≠ sha256(existing body minus
header) → StateModified; body hash == fresh.Hash → StateCurrent; else
StateStale.

Template data: `{Delegate string}` — claude: "Dispatch a search subagent (e.g.
the Task/Agent tool)"; agents: "Delegate to a search subagent if your harness
supports one; otherwise run the bounded probes yourself in order".

**Template content** (`finding-history.md.tmpl`) — this is the deliverable;
write it exactly, with `{{.Delegate}}` where marked:

````text
---
name: agentsview-finding-history
description: Use when asked why a decision was made, how something was done before, or to recover prior instructions, examples, or conversations from recorded agent history — searches the AgentsView archive for evidence.
---

# Finding AgentsView History

Use AgentsView as an evidence source for past agent behavior: find relevant
recorded sessions, inspect the surrounding conversation, and extract
decisions, instructions, or patterns. A snippet is a lead, not an answer.

## Core Workflow

{{.Delegate}}. The coordinator decides what evidence it wants, delegates
the archive crawl, then synthesizes and verifies.

1. Translate the ask into a target behavior and desired evidence: likely
   projects, agents, time windows, vocabulary.
2. Concept questions ("why did we…", "how did we handle…") start with
   hybrid search; exact identifiers (paths, error strings, tool names)
   start with plain or FTS search:

   ```bash
   agentsview session search "<concept query>" --hybrid --context 2 --json --limit 8
   agentsview session search "<exact string>" --fts --json --limit 8
   ```

3. Narrow by recency when the ask implies it: `--since 3m`, `--since 14d`.
   Widen or drop `--since` if results are thin.
4. Triage from the inline `context_before`/`context_after` in the results.
   Deep-dive only the strongest candidates:

   ```bash
   agentsview session messages <session-id> --around <ordinal> --before 8 --after 8 --role user,assistant --json
   ```

5. Give the search a mechanical budget: 4-6 probes, top 2-4 sessions, one
   window per session, then stop and report.
6. Answer from evidence, citing session IDs and ordinals.

## Mode Fallbacks

- "semantic search not available" (HTTP 501): embeddings are not set up on
  this archive. Fall back to FTS probes — several short queries with
  synonyms beat one long phrase:

  ```bash
  agentsview session search "<two or three words>" --fts --json --limit 8
  ```

- "temporarily unavailable" (HTTP 503): the embeddings endpoint is down.
  Retry once; if it persists, say so and continue with FTS — do not
  silently downgrade, the user should know their embeddings are broken.

## Reconstructing a Decision

1. Find the earliest substantive mention: hybrid query for the decision's
   subject, starting `--since 3m` and widening (6m, 1y, none) until the
   origin appears.
2. Walk forward from the origin with message windows. User messages carry
   intent and constraints; assistant messages carry rationale, options
   considered, and tradeoffs.
3. Watch for durable artifacts referenced in the conversation (spec or
   plan documents, ADRs, PR descriptions) and read those files if they
   still exist.
4. Produce a decision record: what was decided, when, by whom, the stated
   reasons, alternatives that were rejected, and citations
   (`session-id` + ordinals) for each claim.

## Rationalization Table

| Rationalization | Reality |
| --- | --- |
| "One good snippet is enough." | Snippets are leads. Inspect a window before extracting a pattern. |
| "A longer query is more semantic." | Hybrid works best with a focused phrase; FTS works best with 2-3 word probes. |
| "Tool output matches are just as good." | Start in messages for concepts; add tool sources only for artifacts, commands, or errors. |
| "This current session mentions it, so it counts." | Down-rank active-session echoes; historical evidence needs older sessions. |
| "Keep searching until certain." | Return a bounded, evidence-backed pass and list follow-ups. |

## Output Shape

```markdown
## Searches
- `query` (mode) -> why it was useful or not

## Strong Matches
- `<session-id>` (`project`, `agent`, ordinals N-M): finding and evidence

## Synthesis
- Decision/pattern grounded in the matches, with citations

## Gaps / Follow-ups
- What was not found and the next narrower probe
```
````

**Steps:**

- [ ] Failing tests: Render for each harness produces parseable frontmatter
  (name `agentsview-finding-history`), contains the harness's Delegate phrase,
  header hash equals recomputed body hash; Classify table
  (missing/current/stale/modified/foreign); TargetDir paths.
- [ ] Implement package; PASS.
- [ ] Commit
  `feat(skills): embedded finding-history skill template and renderer`.

______________________________________________________________________

### Task 3: `skills` command group

**Files:**

- Create: `cmd/agentsview/skills.go`
- Modify: `cmd/agentsview/cli.go` (register)
- Test: `cmd/agentsview/skills_test.go`

**Interfaces:**

- `agentsview skills install [--harness claude|agents]... [--project] [--force]`
  — default both harnesses, user level (`os.UserHomeDir`). `--project`: git
  root of CWD when inside a repo (`git rev-parse --show-toplevel`), else CWD.
  Per target: StateMissing/StateCurrent/StateStale → write (mkdir -p) and
  print `installed <path>` / `up to date <path>` / `updated <path>`;
  StateModified/StateForeign → refuse with
  `"<path> was modified (or not generated); use --force to overwrite"`, exit
  non-zero unless every other target succeeded and `--force` absent semantics:
  refusals make the command exit 1 after processing all targets. `--force`
  overwrites regardless.
- `agentsview skills list [--project]` — table: HARNESS LEVEL STATE PATH using
  skills.Classify against a fresh render; honors `--format json` following the
  repo's existing format-flag convention.
- Version passed from the binary's existing version variable in
  `cmd/agentsview/main.go`.

**Steps:**

- [ ] Failing table-driven install tests over
  (missing/current/stale/modified/foreign) × (--force on/off) with t.TempDir
  as HOME (see how existing tests fake home); list states test; --project test
  using a temp git repo.
- [ ] Implement; PASS. Run `make test-short`.
- [ ] Commit `feat(cli): skills install and list commands`.

______________________________________________________________________

### Task 4: Docs + gate

**Files:**

- Modify: `docs/semantic-search.md` (add "Skills for coding agents" section:
  what install does, harness paths table, re-run after upgrades, note the
  modified-file refusal), `docs/commands.md` (terse `### agentsview skills`
  entry), `docs/semantic-search-internals.md` (one paragraph: generation
  model, hash header, why no plugin)
- Delete: `docs/superpowers/specs/2026-07-05-skills-pack-design.md` and this
  plan file (durable content now lives in the docs)

**Steps:**

- [ ] Write docs; run the repo mdformat hook invocation on
  `docs/semantic-search.md` / internals page (commands.md is excluded from the
  hook — match its existing style manually).
- [ ] `git rm` the spec and plan files.
- [ ] Run `make lint && make test` — the whole-branch gate must be clean.
- [ ] Commit `docs: skills pack usage docs, drop working spec`.

**Checkpoint: invoke the roborev-fix skill after Task 4 and resolve all
findings.**
