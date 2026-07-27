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
[AGENTS.md](https://github.com/kenn-io/agentsview/blob/main/AGENTS.md) is
written for autonomous coding agents. Human contributors do not need to follow
its agent workflow instructions verbatim. Its engineering standards—including
test style, backend parity, localization, content hygiene, and SQLite archive
safety—apply to every contribution regardless of who or what wrote it.

## Before you start

- Read the code around the behavior you want to change and understand the
  boundaries and invariants it already preserves.
- Open an [issue](https://github.com/kenn-io/agentsview/issues) before investing
  heavily in a substantial architecture, storage, public API, or
  user-experience change.
- Keep the contribution focused. Separate unrelated cleanup so reviewers can
  reason about each change on its own.
- Do not expose private names, hostnames, identities, infrastructure details,
  credentials, live data, or absolute machine paths in durable artifacts.

## Build a durable contribution

Code should communicate its responsibilities and failure modes clearly. Follow
existing architecture and naming, prefer focused interfaces, and avoid unrelated
refactoring. Optimize for the engineer who will debug or extend the work months
from now.

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
  is explicitly scoped to one backend. DuckDB remains a disposable read
  mirror.
- Keep every frontend locale synchronized when user-facing messages change.
- Keep background work bounded as stored session counts grow, and add
  cardinality-scaling coverage when changing watcher, polling, or sync paths.
- Reverify provider-format evidence when changing a provider parser or its usage
  and cost accounting.

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

For Go changes, run `go fmt ./...` and `go vet ./...`. For frontend changes, run
`npm run test` and `npm run check` from `frontend/`. Localized frontend changes
also require `npm run i18n:compile`.

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
