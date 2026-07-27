# AgentsView contributing guide design

## Goal

Add a concise public contributing guide that explains AgentsView's engineering
quality bar, how contributors should prepare changes for review, and what to
expect from automated and maintainer review.

The guide must stand on its own. It may adapt general engineering principles
from private internal guidance, but it must not expose private project names,
people, infrastructure, planning systems, or organization-specific operating
details.

## Audience and outcome

The audience is an external contributor who wants to submit a change that is
easy to understand, review, maintain, and merge. After reading the guide, the
contributor should understand:

- what makes a contribution a durable software artifact;
- the expected standards for code, tests, documentation, and validation;
- how to describe a change for a quality-conscious reviewer who did not help
  create it;
- how local roborev feedback can reduce review cycles and maintainer churn; and
- what automated review occurs after a pull request is opened.

## Central quality principle

The guide opens with durability as the organizing standard. A capable engineer
must be able to understand what changed, why it changed, which invariants
matter, and how the behavior is protected using only lasting repository
artifacts.

Code, tests, documentation, commit history, and pull request prose must not
depend on agent transcripts, temporary planning documents, task identifiers,
review-job identifiers, machine-specific paths, or private team context.

This principle leads to the following expectations:

- Code is focused, maintainable, and consistent with the existing architecture.
- Tests cover observable behavior, failure modes, and important invariants
  rather than restating implementation details.
- Documentation and comments preserve decisions future maintainers need.
- Pull request titles describe the durable outcome.
- Pull request descriptions explain the motivation, resulting behavior,
  tradeoffs, limitations, and important review areas in domain language.
- Automated review complements engineering judgment; it does not replace the
  contributor's responsibility for the work.

## Page structure

Create `docs/contributing.md` with these sections:

1. **Contributing to AgentsView** introduces the durability standard and links
   to the repository's public `AGENTS.md` on the default GitHub branch as the
   source of detailed engineering standards. The guide notes that `AGENTS.md`
   is written for autonomous coding agents, while its engineering rules for
   test style, backend parity, localization, content hygiene, archive safety,
   and similar concerns apply to every contribution.
1. **Before you start** asks contributors to understand the affected
   architecture, discuss substantial changes early, and keep scope focused.
1. **Build a durable contribution** covers clear boundaries, preserved
   invariants, tests, safe data handling, storage-backend parity,
   localization, documentation, and maintainable implementation choices.
1. **Validate your work** points contributors to the repository's relevant test,
   formatting, lint, and build commands and asks them to disclose checks they
   could not run.
1. **Review locally with roborev** strongly recommends installing roborev,
   initializing post-commit reviews, optionally installing the Codex agent
   hook, and running a whole-branch review before opening a pull request.
1. **Open a reviewable pull request** explains focused scope and durable,
   rationale-first pull request titles and descriptions.
1. **What happens in review** explains the contributor-visible behavior of
   automated whole-branch roborev review, the current frontier OpenAI
   reviewer, thoughtful resolution of findings, and the role of maintainer
   judgment.

Add **Contributing** immediately after **Quick Start** in `docs/zensical.toml`
so the guide appears near the top of the public documentation navigation.

## Roborev guidance

Local roborev use is a strong recommendation, not a requirement for opening a
pull request and not a contributor-side merge gate. The guide explains that
early adversarial feedback reduces maintainer churn and the time needed to make
a pull request mergeable.

The setup flow links to the official roborev documentation and includes:

```bash
roborev init
roborev skills install
roborev agent-hook install --agent codex
```

The skills and agent-hook steps are optional, but contributors who install the
agent hook must also install the skill it asks Codex to invoke. Contributors may
instead inspect reviews in `roborev tui` and address findings manually.

The recommended pre-pull-request review command is:

```bash
roborev review --branch --agent codex --model gpt-5.6-sol
```

The page dates that exact command "as of July 2026," describes GPT-5.6 Sol as
the current frontier OpenAI model, and links to the official OpenAI model
documentation. It tells readers to check the roborev and OpenAI documentation
for the current frontier model if they read the guide later. The prose also
makes clear that AgentsView may move automated review to a newer frontier OpenAI
model as models change, so the enduring policy is the frontier-model quality bar
rather than a permanent model pin.

Codex and GPT-5.6 Sol are the recommended combination because they match the
project's automated review. They are not prerequisites for useful local review.
The page notes that contributors can use any agent roborev supports and links to
the official supported-agents documentation.

The page does not imply that every automated finding is correct. Contributors
should evaluate findings, fix legitimate issues, and explain or reject
inapplicable findings through the normal review process.

## Contributor-visible automated review

The public guide describes what contributors observe rather than how the review
service is hosted or routed:

- Opening a pull request triggers an automatic whole-branch review of its
  current head.
- `roborev-ci[bot]` posts the synthesized result as a pull request comment,
  usually within several minutes. Queue and reviewer availability can make it
  take longer.
- Pushing new commits triggers a fresh review of the new head, and the result
  identifies the reviewed commit.
- Contributors should fix valid findings in follow-up commits. When a finding is
  inapplicable, they should explain the relevant engineering rationale in the
  pull request conversation so maintainers can evaluate it.
- Automated findings inform review but do not make the merge decision;
  maintainers evaluate the change and the contributor's responses.

The guide does not describe private runners, polling configuration, routing,
credentials, or other implementation details behind the review service.

## Content boundaries

The guide summarizes public contribution expectations rather than duplicating
all standing repository rules. It links to the agent-oriented `AGENTS.md` as the
source of detailed engineering standards, explicitly distinguishes those
standards from agent workflow instructions, and highlights only the rules most
useful to human contributors.

The guide does not prescribe private planning workflows, internal tools other
than the publicly available roborev product, organization-specific staffing or
merge practices, or ephemeral development artifacts.

## Validation

Before committing the implementation:

- format changed Markdown with the pinned docs formatter;
- run the public-docs source checks;
- build the documentation site through the repository's docs validation command
  when local assets and dependencies permit;
- confirm the new page is present near the top of navigation;
- scan the diff for private names, hostnames, identities, absolute paths, and
  internal-only process details; and
- verify every external link and command against current official documentation.

## Acceptance criteria

- `docs/contributing.md` exists and is included immediately after Quick Start in
  public navigation.
- Durability is the page's central engineering standard.
- Code quality, tests, documentation, validation, and pull request prose are
  described in terms useful to an external contributor.
- Local roborev use is strongly recommended and clearly distinguished from
  required repository checks.
- The guide documents `roborev init`, the optional Codex agent hook, and an
  explicit whole-branch review with Codex and GPT-5.6 Sol.
- The model-specific command is dated, links to current official model guidance,
  and is presented as the project-matching recommendation rather than the only
  useful roborev configuration.
- The guide explains the automatic whole-branch pull request review and current
  frontier-model policy without promising a permanent model version, and tells
  contributors where findings appear, when to expect them, and how to respond.
- The guide describes automated review at the contributor-visible behavior level
  without exposing its hosting or routing implementation.
- The page contains no private context or durable references to temporary
  development artifacts.
