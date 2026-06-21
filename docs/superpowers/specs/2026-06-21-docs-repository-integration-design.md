# AgentsView Docs Repository Integration Design

## Goal

Move the AgentsView public documentation from `~/code/agentsview-docs` into this
repository while keeping binary media out of the main branch history and
matching the Vercel ergonomics used by `roborev`.

## Requirements

- Public docs source lives under `docs/` in this repository.
- Vercel treats `docs/` as the project root and uses the docs-local
  `vercel.json`, `pyproject.toml`, `uv.lock`, and `zensical.toml`.
- Screenshot, icon, Open Graph, and diagram media are not committed to `main`.
- Asset publication uses orphan branches:
  - `docs-assets` for curated static assets such as logos, diagrams, icons, and
    Open Graph images.
  - `docs-generated-assets` for generated UI screenshots.
- Docs pages reference hydrated asset paths rather than tracked media paths:
  - `/assets/static/...` for curated static assets.
  - `/assets/generated/...` for generated screenshots.
- Local and Vercel builds hydrate the asset branches before strict Zensical
  builds.
- The existing repository-local `docs/` content is not blindly preserved as a
  competing docs tree. Durable technical documentation can move into a technical
  appendix area of the Zensical docs, and stale planning screenshots or obsolete
  notes can be removed.
- The standalone `agentsview-docs` repository is treated as the source of truth
  for public docs content at migration time.

## Proposed Structure

```text
docs/
  README.md
  assets/
    hydrate-assets.sh
    update-static-assets-branch.sh
  overrides/
    ...
  screenshots/
    ...
    update-generated-assets-branch.sh
  scripts/
    check_built_site.py
    check_vercel_redirects.py
  zensical.toml
  pyproject.toml
  uv.lock
  vercel.json
  vercel-build.sh
  zensical-docs.sh
  *.md
  stylesheets/
  javascripts/
  appendix/
```

This mirrors the `roborev` pattern: the repository root owns developer targets
and deployment helpers, while `docs/` is a self-contained Vercel/Zensical
project. Public Markdown and public docs subdirectories are flattened directly
under `docs/`; they are not nested under `docs/docs/`. The `zensical-docs.sh`
wrapper builds from a temporary public-docs copy so maintainer files such as
`README.md`, scripts, screenshot tooling, and superpowers specs are excluded
from the published site.

The existing `agentsview-docs/overrides` templates must be migrated under the
docs Vercel root as `docs/overrides/` and left wired through
`custom_dir = "overrides"` unless there is an intentional replacement for those
404, header, palette, sitemap, and metadata customizations.

## Asset Ownership

The migration should rewrite existing `agentsview-docs` media references across
all docs source, including Markdown, HTML overrides, CSS, JavaScript, scripts,
and validation checks, as follows:

- `docs/screenshots/*.png` becomes `/assets/generated/screenshots/*.png`.
- `docs/agents/*`, `docs/architecture.svg`, and `docs/og-image.png` become
  `/assets/static/...`.
- `docs/lightbox.js` should move to `docs/javascripts/lightbox.js`, and
  `extra_javascript` should point at `javascripts/lightbox.js`, matching the
  `roborev` convention.

The initial orphan asset branches can be created from the current
`agentsview-docs` media files without preserving the standalone docs repo's blob
history. Future updates to screenshots and curated static assets happen through
the branch update scripts, not by committing media to `main`.

## Existing In-Repo Docs

The current `agentsview/docs/` directory contains technical notes and old
planning screenshots rather than the public docs site. During migration:

- Keep durable docs that remain useful to maintainers, such as release setup,
  route/API notes, DuckDB backend notes, and parser trace notes, under a
  technical appendix path if they build cleanly and are not stale.
- Drop obsolete validation rollout screenshots and transient plan artifacts.
- Do not let legacy files conflict with the imported Zensical docs navigation or
  asset layout.

## Build And Deployment

Root `Makefile` targets should match `roborev` naming:

- `docs-install`
- `docs-build`
- `docs-serve`
- `docs-check`
- `docs-screenshots`
- `docs-assets-branch`
- `docs-generated-assets-branch`
- `docs-deploy-staging`
- `docs-deploy`

`docs/vercel.json` should use:

- `framework: null`
- `installCommand: uv sync --frozen --no-dev`
- `buildCommand: uv run --frozen bash ./vercel-build.sh`
- `outputDirectory: site`
- `trailingSlash: true`

The build script hydrates assets before running a strict Zensical build.

## Validation

Add a docs checker that verifies:

- Repository-root `zensical.toml` and `vercel.json` are absent. The required
  docs-local files are `docs/zensical.toml` and `docs/vercel.json`.
- Image media files are not tracked anywhere in the main-branch docs tree,
  including SVG icons and diagrams and accidentally copied paths such as
  `docs/screenshots`, `docs/agents`, `docs/architecture.svg`, and
  `docs/og-image.png`.
- Nothing under hydrated local asset directories `docs/assets/static` or
  `docs/assets/generated` is tracked.
- Docs media references use `/assets/static/` or `/assets/generated/` in
  Markdown, HTML, CSS, JavaScript, scripts, README content, and metadata checks.
- Assets hydrate before build.
- Strict Zensical build passes.
- Built-site checks and Vercel redirect checks pass.

Ignored local docs outputs should include `docs/.venv/`,
`docs/.zensical-build.*`, `docs/zensical-public-docs.*`, `docs/site/`,
`docs/assets/static/`, `docs/assets/generated/`, `docs/.vercel/`, and docs-local
`.env*.local` files.

Run `make docs-check` before committing the final migration.

## Non-Goals

- Preserve the full Git history of `agentsview-docs`.
- Preserve historical screenshot blobs in this repository's main history.
- Redesign the public docs information architecture beyond the minimal appendix
  needed for useful existing technical docs.
- Change product code behavior.
