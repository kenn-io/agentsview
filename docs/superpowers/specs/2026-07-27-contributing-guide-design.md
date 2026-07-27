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
   to the repository's public `AGENTS.md` on the default GitHub branch for the
   detailed instructions contributors must follow.
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
1. **What happens in review** explains automated whole-branch roborev review,
   the current frontier OpenAI reviewer, thoughtful resolution of findings,
   and the role of maintainer judgment.

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

The page describes GPT-5.6 Sol as the current frontier OpenAI model and links to
the official OpenAI model documentation. The prose also makes clear that
AgentsView may move automated review to a newer frontier OpenAI model as models
change, so the enduring policy is the frontier-model quality bar rather than a
permanent model pin.

The page does not imply that every automated finding is correct. Contributors
should evaluate findings, fix legitimate issues, and explain or reject
inapplicable findings through the normal review process.

## Content boundaries

The guide summarizes public contribution expectations rather than duplicating
all standing repository rules. It links to `AGENTS.md` for detailed,
file-specific requirements and highlights only the rules most useful to human
contributors.

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
- The guide explains the automatic whole-branch pull request review and current
  frontier-model policy without promising a permanent model version.
- The page contains no private context or durable references to temporary
  development artifacts.
