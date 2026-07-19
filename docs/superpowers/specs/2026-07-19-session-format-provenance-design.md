# Session Format Provenance Inventory Design

## Context

Agentsview supports a large and growing set of session providers, but the
evidence used to understand their on-disk or import formats is spread across
parser implementations, fixtures, upstream repositories, vendor documentation,
and reverse-engineering notes. That makes it difficult to audit parsing choices,
especially token-usage and cost attribution, or to refresh a parser when an
upstream format changes.

Create one internal inventory that gives maintainers a reproducible starting
point for every registered provider. The inventory is research documentation,
not a public compatibility promise or a replacement for parser tests.

The `grok` provider is excluded because its format alignment is being handled in
a separate pull request. Every other entry in `internal/parser.Registry` is in
scope, including import-only providers such as Claude.ai and ChatGPT.

## Inventory Location and Structure

Add `docs/internal/session-format-sources.md` as the source of truth. Give every
in-scope provider its own second-level heading containing the registry ID in
backticks. Providers that share an upstream format family still receive distinct
sections so coverage and provider-specific differences remain explicit.

Each provider section records:

- display name and registry ID;
- the on-disk or import layout that Agentsview consumes;
- evidence classification;
- a cloneable upstream repository URL when public source exists;
- a full pinned commit hash and permanent links to relevant files at that
  commit;
- the producer release or format-version range tied to the consumed artifact
  when that relationship can be established, otherwise an explicit statement
  that the revision is only a research snapshot;
- the upstream fields and semantics relevant to input, output, cache, reasoning,
  aggregate, credit, or reported-cost accounting;
- the Agentsview parser files that consume the format; and
- limitations, ambiguity, or absence of usage data.

The inventory may link to the existing Visual Studio Copilot trace note for
additional local detail, but it remains responsible for that provider's upstream
provenance and token-field summary.

## Evidence Policy

Classify every entry as one of:

- `source`: public producer or schema source exists;
- `documentation`: no suitable public producer source was found, but
  authoritative vendor documentation describes the persisted or exported
  format; or
- `no-public-source`: public source and authoritative format documentation were
  exhausted without finding usable evidence.

For `source` entries, prefer producer-side serialization, persistence, schema,
or migration code over examples and downstream consumers. Use the repository's
HTTPS clone URL, a full 40-character commit hash, and file links pinned to that
hash. Verify that every cited path exists at the pinned revision. When one file
defines the session shape and another defines usage accounting, cite both.
Prefer a release-tag commit that can be tied to an observed artifact. A current
`HEAD` revision is acceptable only when its snapshot-only applicability and any
known legacy-generation boundary are stated.

For `documentation` entries, prefer first-party vendor documentation and record
the URL plus the date it was checked. Do not imply that a moving documentation
page is pinned. Public standards may supplement vendor evidence when the vendor
explicitly uses them, but a general standard alone does not prove a provider's
persisted format.

For `no-public-source` entries, state that no usable public format source was
found and summarize the first-party repositories or documentation surfaces that
were checked. Sanitized local observations and the Agentsview parser may explain
current behavior, but must be labeled as implementation evidence rather than
upstream authority. Record exact URLs or organizations, useful pinned revisions,
the repeated format/persistence and token/cost search terms, and the check date
so the negative conclusion can be reproduced.

Treat the evidence class as the strongest public authority present, not blanket
support for every claim in a section. Generic standards and documentation that
only establishes an export artifact are supplemental rather than proof of a
complete persisted schema. Give every entry a last-verified date and reverify it
during provider-release investigations, new artifact generations, parser or
usage-accounting bug reports, and periodic inventory review. If evidence
disappears, retain its original URL and commit identity while adding an archive
or maintained mirror.

Token and cost notes must distinguish fields directly persisted by the provider
from values Agentsview derives later using its pricing catalog. Explicitly state
when a format exposes no token usage, reports only aggregate usage, uses AI
credits instead of currency, or stores a provider-reported monetary cost.

## Coverage Enforcement

Add a focused Go test in `internal/parser` that reads provider IDs from the
inventory's second-level headings and compares them with
`internal/parser.Registry` in both directions after excluding `AgentGrok`. The
test fails for missing entries, unknown entries, and duplicate provider IDs.

The test also enforces exactly one of each required field in the prescribed
order, the evidence-class vocabulary, full source revisions and pinned links,
and check dates for documentation or negative-source entries. It does not parse
or freeze prose conclusions. Research quality remains a review concern, while
the automated contract prevents structurally incomplete entries from appearing
covered.

## Maintainer Rule

Add a rule to the root `AGENTS.md` requiring implementers who add or change a
provider, or investigate a provider release, new artifact generation, parser
bug, or usage discrepancy, to consult `docs/internal/session-format-sources.md`
and reverify or update its evidence entry in the same change.

Grok remains temporarily excluded because its separately owned format-alignment
work is in progress. The exit criterion is that work landing: then add Grok to
the inventory and remove the explicit registry exception.

Do not modify the frontend-specific `AGENTS.md`; the rule concerns parser and
provider maintenance across the repository.

## Validation

- Format the new Markdown with `mdformat --wrap 80` when the configured Markdown
  tooling is available.
- Run the focused inventory-coverage test with the repository's required Go
  build settings.
- Run the existing Markdown source check and any broader parser tests practical
  for the documentation and test changes.
- Review the final diff for private project names, hostnames, personal identity,
  infrastructure details, and absolute local paths.
- Do not change parser behavior, pricing behavior, provider registration, or the
  Grok inventory in this work.

## Deliverable

Commit the design separately, then implement the inventory, coverage test, and
root maintenance rule as focused follow-up work on the approved feature branch.
Push and pull-request creation remain separate actions requiring the user's
request under the repository Git rules.
