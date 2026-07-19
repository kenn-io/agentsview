# Session Format Provenance Inventory Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a reproducible source inventory for every registered session
provider except Grok, with special attention to token-usage and cost fields.

**Architecture:** A human-readable internal Markdown document is the source of
truth. A focused Go test extracts provider IDs from its headings and enforces
exact two-way coverage against `parser.Registry`, while a root `AGENTS.md` rule
keeps the inventory in the provider-development workflow.

**Tech Stack:** Markdown, Go 1.26.3, `testify`, Git, first-party source
repositories, and first-party vendor documentation.

## Global Constraints

- Exclude `AgentGrok`; its format alignment belongs to a separate pull request.
- Include all other entries in `internal/parser.Registry`, including import-only
  Claude.ai and ChatGPT formats.
- Give each provider its own `` ## Display Name (`provider-id`) `` heading even
  when formats are shared.
- Classify evidence as `source`, `documentation`, or `no-public-source`.
- Source-backed entries use an HTTPS clone URL, a full 40-character commit hash,
  and permanent file links verified at that revision.
- Documentation-backed entries use first-party URLs and record the check date as
  `2026-07-19`.
- Distinguish persisted token/cost data from Agentsview-computed pricing.
- State explicitly when token usage, cost, cache, reasoning, aggregate, or
  credit data is absent or ambiguous.
- Do not change parser behavior, pricing behavior, or provider registration.
- Do not include private project names, hostnames, personal identities,
  infrastructure details, or absolute local paths in committed content.
- Do not push, pull, rebase, amend, or open a pull request without a separate
  explicit request.

______________________________________________________________________

## File Map

- Create `docs/internal/session-format-sources.md`: research inventory and sole
  human-maintained source of provider format provenance.
- Create `internal/parser/format_sources_test.go`: exact registry-minus-Grok
  coverage enforcement for inventory headings.
- Modify `AGENTS.md`: durable instruction to consult and update the inventory
  during provider work.

### Task 1: Add the registry coverage contract

**Files:**

- Create: `internal/parser/format_sources_test.go`
- Read: `internal/parser/types.go`
- Read: `internal/parser/types_test.go`

**Interfaces:**

- Consumes: `parser.Registry`, `parser.AgentGrok`, and second-level inventory
  headings shaped as `` ## Display Name (`provider-id`) ``.

- Produces: `TestSessionFormatSourcesCoverRegistry`, which fails for a missing,
  unknown, or duplicate provider ID.

- [ ] **Step 1: Write the failing coverage test**

Create `internal/parser/format_sources_test.go` with:

```go
package parser

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var formatSourceHeadingRE = regexp.MustCompile(
	"(?m)^## [^\\n]+ \\(`([a-z0-9-]+)`\\)$",
)

func TestSessionFormatSourcesCoverRegistry(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve format inventory test path")

	inventoryPath := filepath.Join(
		filepath.Dir(testFile), "..", "..", "docs", "internal",
		"session-format-sources.md",
	)
	raw, err := os.ReadFile(inventoryPath)
	require.NoError(t, err)

	documented := make(map[AgentType]bool)
	for _, match := range formatSourceHeadingRE.FindAllSubmatch(raw, -1) {
		agent := AgentType(match[1])
		assert.Falsef(t, documented[agent],
			"provider %q documented more than once", agent)
		documented[agent] = true
	}

	expected := make(map[AgentType]bool, len(Registry)-1)
	for _, def := range Registry {
		if def.Type == AgentGrok {
			continue
		}
		expected[def.Type] = true
	}

	for agent := range documented {
		assert.Truef(t, expected[agent],
			"inventory documents unknown or excluded provider %q", agent)
	}
	for agent := range expected {
		assert.Truef(t, documented[agent],
			"provider %q missing from format inventory", agent)
	}
}
```

- [ ] **Step 2: Format the test**

Run:

```bash
gofmt -w internal/parser/format_sources_test.go
```

Expected: the file is formatted with no output.

- [ ] **Step 3: Run the test to establish the red state**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/parser \
  -run '^TestSessionFormatSourcesCoverRegistry$' -count=1
```

Expected: FAIL because `docs/internal/session-format-sources.md` does not exist.

### Task 2: Research and author the complete inventory

**Files:**

- Create: `docs/internal/session-format-sources.md`
- Read: `internal/parser/*.go`
- Read: `internal/parser/testdata/**`
- Read: `docs/internal/visual-studio-copilot-traces.md`

**Interfaces:**

- Consumes: the evidence policy in
  `docs/superpowers/specs/2026-07-19-session-format-provenance-design.md` and
  the heading parser from Task 1.

- Produces: 50 independently useful provider sections whose heading IDs exactly
  match the registry minus `grok`.

- [ ] **Step 1: Create the inventory preamble and evidence template**

Start `docs/internal/session-format-sources.md` with this contract:

```markdown
# Session Format Source Inventory

This inventory records the best reproducible evidence currently available for
the session formats consumed by Agentsview. It is a maintainer research aid, not
a compatibility guarantee. Source links are pinned; documentation links are
moving first-party pages and include the date checked.

Evidence classes:

- `source`: public producer, persistence, schema, or migration source.
- `documentation`: first-party format documentation without suitable public
  producer source.
- `no-public-source`: no usable public source or authoritative format
  documentation was found after the searches recorded in the entry.

Usage notes distinguish values persisted by the provider from costs Agentsview
computes later with its pricing catalog.
```

Use this exact field order in every provider section:

```markdown
## Display Name (`provider-id`)

- **Format:** Files, database tables, or import artifact consumed by Agentsview.
- **Evidence:** `source`, `documentation`, or `no-public-source`.
- **Upstream:** HTTPS clone URL plus full revision and pinned file links; or
  first-party documentation plus `checked 2026-07-19`; or the public surfaces
  searched without success.
- **Usage and cost:** Persisted token, cache, reasoning, aggregate, credit, and
  reported-cost fields, including what is absent and what Agentsview derives.
- **Agentsview:** Relative parser source paths and any format limitations.
```

- [ ] **Step 2: Add all canonical provider headings before research**

Add one section, in registry order, for each of these exact IDs:

```text
claude
openclaude
cowork
codex
copilot
gemini
mimocode
opencode
kilo
openhands
cursor
amp
vscode-copilot
windsurf
visualstudio-copilot
pi
omp
qwen
commandcode
deepseek-tui
openclaw
qclaw
kimi
claude-ai
chatgpt
kiro
kiro-ide
cortex
hermes
forge
devin
piebald
warp
positron
posit-assistant
zcode
zed
antigravity
antigravity-cli
iflow
icodemate
workbuddy
zencoder
gptme
qoder
qwenpaw
shelley
vibe
aider
reasonix
```

Do not add a `grok` heading.

- [ ] **Step 3: Build an implementation-derived format map**

For each heading, inspect its parser and provider source with:

```bash
rg -n 'json|jsonl|sqlite|\.db|history|session|message|usage|token|cache|cost|credit|reasoning' \
  internal/parser --glob '*.go' --glob '!**/*_test.go'
```

Record the concrete source layout and Agentsview parser paths first. For format
families, inspect all provider-specific differences rather than copying one
entry unchanged. At minimum, compare these known families:

- Claude Code, OpenClaude, and Cowork;

- OpenCode, MiMoCode, Kilo, and IcodeMate;

- Copilot CLI, VS Code Copilot, and Visual Studio Copilot;

- Pi, OhMyPi, and Reasonix;

- OpenClaw, QClaw, and QwenPaw;

- Kiro CLI and Kiro IDE;

- Antigravity IDE and Antigravity CLI; and

- VS Code-derived stores used by Windsurf and Positron.

- [ ] **Step 4: Verify public-source evidence at immutable revisions**

For every provider with a plausible public producer repository:

1. Prefer the first-party repository over forks or reverse-engineering tools.
1. Resolve a full revision with `git ls-remote <https-clone-url> HEAD`.
1. Inspect the repository tree or a filtered clone for persistence/schema files.
1. Verify each cited path with `git cat-file -e <revision>:<path>`.
1. Cite a permanent `blob/<full-revision>/<path>` link.
1. Cite separate files when session shape and usage accounting are defined in
   different locations.

Start with the known first-party projects represented by Codex, Gemini CLI,
OpenCode, OpenHands, Pi, Qwen Code, Hermes Agent, Zed, Positron, Posit
Assistant, gptme, Mistral Vibe, and Aider, then follow first-party package or
repository links found in the remaining provider metadata and documentation.
Treat this list as a search starting point, not evidence by itself.

- [ ] **Step 5: Verify documentation evidence for closed-source formats**

Search first-party vendor documentation and public organization repositories for
every entry without suitable producer source. Prefer export-format, telemetry,
storage, schema, or session-history pages over general product docs. Record
`checked 2026-07-19` beside each moving documentation URL.

For Claude.ai and ChatGPT, document the exported archive consumed by the import
parser rather than a local application store. For OpenTelemetry-derived Visual
Studio Copilot data, cite the vendor evidence for emitted attributes and the
applicable semantic conventions, while retaining the existing local trace note
as implementation detail.

- [ ] **Step 6: Record exhausted searches honestly**

When neither producer source nor authoritative format documentation is public,
use `no-public-source`. Name the first-party organization repositories,
documentation sites, or product pages checked, then describe current parser
behavior as Agentsview implementation evidence. Do not elevate fixtures,
third-party blog posts, or inferred field names to upstream authority.

- [ ] **Step 7: Audit token and cost semantics provider by provider**

For every section, answer all applicable questions explicitly:

- Are input and output tokens persisted per message, per request, or only as an
  aggregate?
- Are cache creation/read, reasoning, or context token fields persisted?
- Is monetary cost persisted by the provider, are AI credits persisted, or does
  Agentsview compute price later?
- Does the parser intentionally expose no per-message token data?
- Are usage fields cumulative counters requiring deltas or independent events?
- Are model IDs present where pricing needs them?

Cross-check the resulting statements against the parser's actual field reads:

```bash
rg -n 'TokenUsage|UsageEvents|InputTokens|OutputTokens|ContextTokens|CostUSD|cost|credits|cached|reasoning' \
  internal/parser --glob '*.go' --glob '!**/*_test.go'
```

- [ ] **Step 8: Run the focused coverage test to reach green**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/parser \
  -run '^TestSessionFormatSourcesCoverRegistry$' -count=1
```

Expected: PASS with all 50 in-scope providers documented exactly once.

### Task 3: Add the durable provider-maintenance rule

**Files:**

- Modify: `AGENTS.md`, after the `## Backend Parity` section
- Read: `frontend/AGENTS.md` only to confirm it remains untouched

**Interfaces:**

- Consumes: `docs/internal/session-format-sources.md` from Task 2.

- Produces: a standing repository instruction that keeps format provenance
  synchronized with provider implementation changes.

- [ ] **Step 1: Add the root instruction**

Add this one-line paragraph to the root `AGENTS.md` under a new
`## Provider Format Provenance` heading:

```markdown
## Provider Format Provenance

When adding a provider or changing a provider's on-disk format or usage/cost
accounting, consult `docs/internal/session-format-sources.md` and update its
evidence entry in the same change.
```

- [ ] **Step 2: Confirm the frontend instruction file is unchanged**

Run:

```bash
git diff -- frontend/AGENTS.md
```

Expected: no output.

### Task 4: Format, validate, review, and commit the implementation

**Files:**

- Modify: formatting only in the three implementation files from Tasks 1-3
- Validate: `docs/internal/session-format-sources.md`
- Validate: `internal/parser/format_sources_test.go`
- Validate: `AGENTS.md`

**Interfaces:**

- Consumes: the completed inventory, coverage test, and maintenance rule.

- Produces: one focused implementation commit ready for review.

- [ ] **Step 1: Format Go and Markdown**

Run:

```bash
gofmt -w internal/parser/format_sources_test.go
cd docs && uv run --frozen mdformat --wrap 80 \
  internal/session-format-sources.md ../AGENTS.md
```

Expected: formatting succeeds and changes only the intended files.

- [ ] **Step 2: Run required Go verification**

Run:

```bash
go fmt ./...
CGO_ENABLED=1 go test -tags fts5 ./internal/parser \
  -run '^TestSessionFormatSourcesCoverRegistry$' -count=1
go vet ./...
```

Expected: all commands pass.

- [ ] **Step 3: Run broader practical checks**

Run:

```bash
python3 docs/scripts/check_markdown_sources.py
make test-short
```

Expected: the Markdown source check and short test suite pass. If the broader
suite fails for an unrelated environment issue, preserve the exact output and
report it rather than weakening the check.

- [ ] **Step 4: Check evidence mechanics and content hygiene**

Confirm every `source` entry has a clone URL, full 40-character revision, and at
least one pinned file link. Re-run `git cat-file -e` for every cited path in the
corresponding temporary clone. Confirm every `documentation` entry includes
`checked 2026-07-19`, every `no-public-source` entry names the searches made,
and all 50 sections discuss usage/cost availability.

Run:

```bash
git diff --check
git diff --stat
git diff -- AGENTS.md docs/internal/session-format-sources.md \
  internal/parser/format_sources_test.go
```

Expected: no whitespace errors, only the three intended implementation files, no
private data, no absolute local paths, and no Grok section.

- [ ] **Step 5: Stage only implementation files**

Run:

```bash
git add AGENTS.md docs/internal/session-format-sources.md \
  internal/parser/format_sources_test.go
git diff --cached --check
git diff --cached --stat
```

Expected: exactly the inventory, coverage test, and root maintenance rule are
staged.

- [ ] **Step 6: Commit through the mandatory commit workflow**

After invoking the mandatory commit skill, create a conventional commit without
generated attribution lines, as required by the repository instructions:

```bash
git commit -m "docs: inventory provider session format sources" \
  -m "Provider parsing and usage accounting need reproducible upstream evidence so format changes can be audited without rediscovering source schemas and closed-source limitations. The inventory pins public source, records authoritative documentation or exhausted searches, and enforces registry coverage while leaving prose conclusions reviewable."
```

Expected: hooks pass and one non-empty implementation commit is created. Do not
push or open a pull request unless the user explicitly requests those actions.

## Final Verification Checklist

- [ ] The design commit remains separate from the implementation commit.
- [ ] The inventory has exactly 50 provider headings and excludes `grok`.
- [ ] Every public-source citation resolves at its full pinned revision.
- [ ] Every provider section explains token and cost availability or absence.
- [ ] `TestSessionFormatSourcesCoverRegistry` passes.
- [ ] `AGENTS.md` requires future provider implementers to maintain the
  inventory.
- [ ] Go formatting, Go vet, focused tests, Markdown checks, and practical
  broader tests have recorded results.
- [ ] The worktree is clean after the implementation commit.
