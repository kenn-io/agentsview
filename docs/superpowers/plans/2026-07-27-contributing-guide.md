# AgentsView Contributing Guide Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish a concise contributor guide that makes durability the
AgentsView engineering quality bar and explains the local and hosted roborev
review experience.

**Architecture:** Add one public Markdown page and one Zensical navigation
entry. Keep the guide focused on durable repository artifacts and
contributor-visible behavior; link to detailed engineering and product sources
instead of duplicating agent workflows or private review infrastructure.

**Tech Stack:** Markdown, Zensical TOML navigation, `mdformat`, and the
repository docs validation scripts.

## Global Constraints

- The guide must stand on its own and contain no private project names, people,
  infrastructure, planning systems, or organization-specific operating
  details.
- Durability is the organizing standard for code, tests, documentation, commit
  history, and pull request prose.
- `AGENTS.md` is an agent-oriented source of detailed engineering standards, not
  a workflow that human contributors must follow verbatim.
- Local roborev use is a strong recommendation, not a pull request requirement
  or contributor-side merge gate.
- The Codex and GPT-5.6 Sol command is dated "as of July 2026" and presented as
  the configuration that matches project automation, not the only useful local
  configuration.
- Contributors may use any agent roborev supports.
- Hosted review is described only through contributor-visible pull request
  behavior; do not document runners, polling configuration, routing,
  credentials, or hosting.
- Do not deploy the documentation. The requested deliverable is committed source
  and navigation.

______________________________________________________________________

### Task 1: Publish the public contributing guide

**Files:**

- Create: `docs/contributing.md`
- Modify: `docs/zensical.toml`
- Delete before the final commit:
  `docs/superpowers/specs/2026-07-27-contributing-guide-design.md`
- Delete before the final commit:
  `docs/superpowers/plans/2026-07-27-contributing-guide.md`

**Interfaces:**

- Consumes: the engineering rules in `AGENTS.md`, the public navigation array in
  `docs/zensical.toml`, and current official roborev and OpenAI documentation.

- Produces: a public `/contributing/` documentation route and a **Contributing**
  navigation item immediately after **Quick Start**.

- [ ] **Step 1: Reverify the time-sensitive review facts**

Run:

```bash
curl -fsSL https://www.roborev.io/guides/reviewing-code.md \
  | rg -n -m 3 'review --branch|--agent|--model'
curl -fsSL https://www.roborev.io/automation/post-commit-reviews.md \
  | rg -n -m 5 'roborev init|skills install|agent-hook install'
curl -fsSL https://www.roborev.io/agents/ \
  | rg -n -m 3 'Codex|Claude Code|Gemini'
curl -fsSL https://developers.openai.com/api/docs/models/gpt-5.6-sol \
  | rg -n -m 1 'Frontier model for complex professional work'
gh api repos/kenn-io/agentsview/issues/1280/comments \
  --jq '.[] | select(.user.login == "roborev-ci[bot]") |
    [.created_at, (.body | split("\n")[1])] | @tsv'
```

Expected:

- the roborev sources show branch review, post-commit initialization, skill,
  agent-hook, and multiple supported-agent guidance;
- the OpenAI source identifies GPT-5.6 Sol as the frontier professional-work
  model; and
- the live AgentsView pull request shows `roborev-ci[bot]` combined-review
  comments.

If a time-sensitive fact has changed, update the draft below to match the
current official source while preserving the frontier-model quality policy.

- [ ] **Step 2: Create the complete public guide**

Create `docs/contributing.md` with this content:

````markdown
---
title: Contributing
description: Engineering standards and review workflow for AgentsView contributions
---

# Contributing to AgentsView

We welcome changes that leave AgentsView easier to understand, safer to change,
and more useful than before.

Our central quality standard is **durability**. A capable engineer should be
able to understand what changed, why it changed, which invariants matter, and
how the behavior is protected using only the repository's lasting artifacts.
They should not need the original agent session, temporary planning documents,
task identifiers, review-job identifiers, machine-specific paths, or private
context.

The repository's
[AGENTS.md](https://github.com/kenn-io/agentsview/blob/main/AGENTS.md) is written
for autonomous coding agents. Human contributors do not need to follow its
agent workflow instructions verbatim. Its engineering standards—including test
style, backend parity, localization, content hygiene, and SQLite archive
safety—apply to every contribution regardless of who or what wrote it.

## Before you start

- Read the code around the behavior you want to change and understand the
  boundaries and invariants it already preserves.
- Open an [issue](https://github.com/kenn-io/agentsview/issues) before investing
  heavily in a substantial architecture, storage, public API, or user-experience
  change.
- Keep the contribution focused. Separate unrelated cleanup so reviewers can
  reason about each change on its own.
- Do not expose private names, hostnames, identities, infrastructure details,
  credentials, live data, or absolute machine paths in durable artifacts.

## Build a durable contribution

Code should communicate its responsibilities and failure modes clearly. Follow
existing architecture and naming, prefer focused interfaces, and avoid
unrelated refactoring. Optimize for the engineer who will debug or extend the
work months from now.

Tests are part of the artifact, not evidence attached after the fact. New
features and bug fixes need tests that exercise observable behavior, important
failure modes, and invariants. Avoid tests that merely duplicate the
implementation. Use the repository's established test helpers and assertion
style.

Preserve the project's cross-cutting contracts:

- Treat the SQLite archive as persistent user data. Schema and parser changes
  must preserve existing sessions through the migration and resync paths
  documented in `AGENTS.md`.
- Keep SQLite and PostgreSQL behavior and query shape aligned unless the change
  is explicitly scoped to one backend. DuckDB remains a disposable read mirror.
- Keep every frontend locale synchronized when user-facing messages change.
- Keep background work bounded as stored session counts grow, and add
  cardinality-scaling coverage when changing watcher, polling, or sync paths.
- Reverify provider-format evidence when changing a provider parser or its
  usage and cost accounting.

Documentation, comments, schemas, tests, error messages, and metadata should
explain the domain responsibility or invariant they preserve. Do not make
durable artifacts depend on temporary task names, planning trees, agent
transcripts, or review history.

## Validate your work

Run the checks relevant to the files and behavior you changed. Common commands
include:

```bash
make test-short
make test
make vet
make lint
make e2e
make docs-check
```

For Go changes, run `go fmt ./...` and `go vet ./...`. For frontend changes,
run `npm run test` and `npm run check` from `frontend/`. Localized frontend
changes also require `npm run i18n:compile`.

Some integration suites require external services or platform-specific
environments. If you cannot run a relevant check, say so plainly when handing
off the change. Never claim a check passed without its current output.

## Review locally with roborev

We strongly recommend using [roborev](https://www.roborev.io/) during
development. Early adversarial feedback catches correctness, testing, and
maintainability problems while the change is still fresh. Resolving those
findings before opening a pull request reduces maintainer churn and usually
shortens the path to a mergeable change.

After [installing roborev](https://www.roborev.io/installation/), initialize it
inside your checkout:

```bash
roborev init
```

This installs the post-commit review hook. Inspect findings with `roborev tui`
and fix them manually if you prefer.

Codex users can optionally close the write-review-fix loop automatically:

```bash
roborev skills install
roborev agent-hook install --agent codex
```

The agent hook expects the installed roborev fix skill. It prompts Codex to
address accumulated findings before the session context goes cold.

Before opening a pull request, we recommend one review of the complete branch.
As of July 2026, this command matches the project's automated reviewer:

```bash
roborev review --branch --agent codex --model gpt-5.6-sol
roborev tui
```

[GPT-5.6 Sol](https://developers.openai.com/api/docs/models/gpt-5.6-sol) is the
current frontier OpenAI model. Model names change, so check the current roborev
and OpenAI documentation if this dated example becomes stale.

Codex and OpenAI access are not prerequisites for useful local review. roborev
supports [multiple coding agents](https://www.roborev.io/agents/); use any
supported agent available in your environment. The explicit Codex configuration
is recommended because it most closely matches the review AgentsView runs after
you open a pull request.

Local roborev use is a strong recommendation, not a requirement for opening a
pull request and not a contributor-side merge gate. Automated findings also
require judgment: fix legitimate issues and do not make speculative changes
solely to satisfy an inapplicable finding.

## Open a reviewable pull request

Keep the pull request focused enough that a reviewer can understand its purpose
and risk. The title should describe the durable outcome, not the activity used
to produce it.

Write the description for a quality-conscious engineer who did not participate
in creating the change. Explain:

- why the change is needed;
- what the code does now;
- important design decisions and preserved invariants;
- tradeoffs and known limitations; and
- where reviewer attention is most valuable.

Do not require reviewers to reconstruct the rationale from the diff, an agent
transcript, temporary plans, or task and review identifiers. Do not add a test
plan or verification checklist to the pull request description; CI reports the
standard checks directly.

## What happens in review

Opening a pull request triggers an automatic whole-branch roborev review of its
current head. `roborev-ci[bot]` posts the synthesized result as a pull request
comment, usually within several minutes. Queue and reviewer availability can
make it take longer. Pushing new commits triggers a fresh review of the new
head, and each result identifies the commit it reviewed.

Treat the comment like other review feedback. Fix valid findings in follow-up
commits. When a finding is inapplicable, explain the engineering rationale in
the pull request conversation so maintainers can evaluate it.

Automated review complements human judgment; it does not make the merge
decision. Maintainers evaluate the change, the review findings, and the
contributor's responses together.
````

Expected: the page contains every approved section, uses only public links and
behavior, and distinguishes engineering standards, recommendations, and review
gates.

- [ ] **Step 3: Add the guide near the top of public navigation**

In `docs/zensical.toml`, change the start of `nav` to:

```toml
nav = [
  {"Quick Start" = "quickstart.md"},
  {"Contributing" = "contributing.md"},
  {"Configuration" = "configuration.md"},
```

Expected: **Contributing** appears immediately after **Quick Start** and before
**Configuration**.

- [ ] **Step 4: Format and inspect the public source**

Run:

```bash
uvx --from mdformat==0.7.22 \
  --with mdformat-frontmatter \
  --with mdformat-mkdocs \
  --with mdformat-tables==1.0.0 \
  mdformat --wrap 80 --align-semantic-breaks-in-lists \
  docs/contributing.md
prek run mdformat --files docs/contributing.md --verbose
git diff --check
```

Expected:

- `mdformat` exits successfully;

- the targeted pre-commit hook reports `Passed`; and

- `git diff --check` emits no output.

- [ ] **Step 5: Verify requirements and scrub private context**

Run:

```bash
rg -n '^# Contributing to AgentsView$' docs/contributing.md
rg -n '^  \{"Contributing" = "contributing.md"\},$' docs/zensical.toml
rg -n 'roborev review --branch --agent codex --model gpt-5\.6-sol' \
  docs/contributing.md
rg -n 'as of July 2026|As of July 2026' docs/contributing.md
rg -n 'roborev-ci\[bot\]|usually within several minutes' \
  docs/contributing.md
! rg -n '(/Users/|~/code/|kwiki|How Kenn Builds|private runner|poll_interval|github_app_)' \
  docs/contributing.md
```

Expected:

- each required content search returns at least one match;

- the private-context scrub returns no matches; and

- the model example is dated rather than presented as a permanent pin.

- [ ] **Step 6: Validate the built documentation**

Run:

```bash
AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1 make docs-check
```

Expected: source validation, Zensical build, built-site validation, and redirect
validation all complete with exit code 0. `make docs-check` includes the public
documentation build, so a separate deployment is neither needed nor authorized.

- [ ] **Step 7: Remove temporary planning artifacts**

Run:

```bash
git rm \
  docs/superpowers/specs/2026-07-27-contributing-guide-design.md \
  docs/superpowers/plans/2026-07-27-contributing-guide.md
git diff --check
git status --short
```

Expected: the final tree contains only the durable contributor page and
navigation change; the design and implementation plan are staged for deletion.

- [ ] **Step 8: Commit the durable documentation**

First invoke the mandatory `kenn:commit` skill. Then run:

```bash
git add docs/contributing.md docs/zensical.toml
git commit -m "docs: add contributor engineering guide" \
  -m "External contributors need a public quality bar that explains how to leave maintainable artifacts and prepare changes for efficient review. The guide makes durability the organizing principle and documents both optional local roborev feedback and the contributor-visible hosted review experience."
```

Expected: the commit succeeds with the repository hooks enabled and includes the
new page, navigation entry, and deletion of the temporary design and plan.
