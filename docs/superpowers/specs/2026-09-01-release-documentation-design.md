# Release Documentation Refresh Design

## Goal

Make the 0.42.0 release easy to understand and keep future changelog entries
focused on what changes for users. Update the product site and documentation so
they describe the released product accurately, including its optional hosted
sync path.

## Approach

Keep the new site's visual system, routes, and nine-part product story. Rewrite
the content around user outcomes and correct facts that changed in 0.42.0. This
is broader than a release-note patch but does not redesign the site.

Two other approaches were considered:

- A facts-only patch would be smaller, but it would leave internal terms and
  implementation-led changelog entries in place.
- A full site redesign could change the product story, but it would add visual
  and navigation risk without helping people understand this release.

## Marketing hierarchy

The hero headline states the immediate outcome: people can find their coding
agent sessions, answers, and costs in one place. The supporting line identifies
AgentsView as the local-first system of record for coding-agent sessions.

The rest of the page keeps the current progression from capture through search,
cost, knowledge, sharing, and ownership. Copy leads with the result for the
reader. Product architecture and implementation details follow only when they
help explain that result.

The site must describe privacy precisely. AgentsView keeps data local by
default. Data leaves the machine only when the user configures a remote target,
including hosted raw sync. The site must not say that the product has no cloud
services now that an optional hosted path is released.

## Changelog scope and style

Replace the current Unreleased section with a dated 0.42.0 entry based on the
released feature set. Review and rewrite every release from 0.36.0 on 2026-07-02
through 0.42.0 on 2026-09-01.

Keep version headings, dates, categories, facts, limitations, issue references,
and acknowledgements. Each bullet starts with the outcome a user or operator
will notice. Use a second sentence only when a boundary, migration, opt-in, or
technical term changes what the reader should do. Keep command names, config
keys, providers, and storage engines when those names help a reader act.

Add a durable rule to `AGENTS.md`: changelog entries must use plain language,
lead with human outcomes, and put implementation detail after the user-visible
change.

## Documentation scope

Review the public Zensical pages against 0.42.0 and update the pages affected by
the release. The review includes:

- hosted raw sync authentication, resumable upload, checkpointing, consent,
  continuous watching, and operational boundaries;
- exact one-shot capture usage and durable raw artifacts;
- Cursor IDE, ICodeMate, and Posit Assistant session and usage coverage;
- S3 discovery across supported single-file providers;
- Japanese localization;
- historical pricing and OpenRouter coverage;
- optional source paths in API responses;
- candidate-finding policy for Recall;
- usage-report performance and other user-visible 0.42 improvements and fixes.

Navigation labels, overview pages, quick starts, privacy text, cross-links, and
machine-readable Markdown twins must agree with the detailed pages. Do not claim
support beyond the implementation or remove documented limits.

## Screenshots

Regenerate the full Playwright screenshot set from the privacy-filtered fixture.
Review each outgoing PNG at original resolution and inspect its metadata before
updating the local `docs-generated-assets` orphan branch. Do not push the asset
branch without a separate request.

Use the refreshed screenshots in the existing marketing and guide layouts. Check
the landing page, guide, and Zensical documentation at desktop and mobile widths
after the assets are hydrated.

## Verification

Run the repository's documentation build and checks, Markdown formatting, and
focused tests for any validator or screenshot-pipeline code that changes.
Inspect the assembled site and generated screenshots rather than relying only on
source checks. Scan changed public content and outgoing images for private data
before committing or publishing.

Continue monitoring the 0.42.0 Release, Desktop Release, and Docker workflows
until all three finish. If a workflow fails, inspect the failed job before
making any claim about the cause or proposing a fix.
