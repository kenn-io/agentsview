# AgentsView docs maintainer guide

This directory contains the source for <https://agentsview.io>: a hand-written
marketing tier (`/` and `/guide/`) plus the Zensical documentation tier under
`/docs/`. The source lives on `main`; image media lives on orphan asset branches
so normal clones do not pull screenshots and PNGs into the main history.

## Site structure

- `/` and `/guide/`: static marketing pages from `website/`.
- `/docs/**`: Zensical-rendered documentation from the top-level `*.md` pages.
- Every published page has a raw Markdown twin: `/index.md`, `/guide.md`,
  `/docs/index.md`, and `/docs/<page>.md` beside each `/docs/<page>/` route.
- `/llms.txt`: hand-maintained machine-readable index of the Markdown twins. Add
  every new public page to `zensical.toml`, `llms.txt`, and
  `scripts/check_built_site.py` (`DOCS_PAGES`), plus the redirect tables if it
  replaces an old URL.
- Legacy root docs URLs (`/quickstart/`, `/usage.md`, `/assets/...`) redirect
  permanently to their `/docs/` equivalents via `vercel.json`.

## Layout

- `*.md`: public docs source, rendered under `/docs/`.
- `website/`: marketing tier (HTML, CSS, fonts, Markdown twins) copied to the
  site root at build time.
- `llms.txt`: machine-readable page index published at the site root.
- `agents/`, `internal/`, and `superpowers/`: maintainer references excluded
  from the published site.
- `zensical.toml`: Zensical site configuration and navigation.
- `pyproject.toml` and `uv.lock`: pinned docs toolchain.
- `vercel.json` and `vercel-build.sh`: Vercel project configuration, including
  the legacy-URL redirect table.
- `zensical-docs.sh`: builds from a temporary public-docs copy so maintainer
  files are excluded, then assembles the website tier, Markdown twins,
  `llms.txt`, and root sitemap into `site/`.
- `assets/hydrate-assets.sh`: hydrates ignored local assets from orphan
  branches.
- `assets/update-static-assets-branch.sh`: updates curated static assets.
- `screenshots/`: Docker/Playwright screenshot generator and generated asset
  branch updater.
- `scripts/check_built_site.py` and `scripts/check_vercel_redirects.py`:
  post-build validation.

`docs/assets/static/`, `docs/assets/generated/`, `docs/site/`, `docs/.venv/`,
and `docs/zensical-public-docs.*` are ignored local outputs.

## Asset Branches

- `docs-assets`: curated static media, including the architecture diagram and
  Open Graph image.
- `docs-generated-assets`: generated UI screenshots.

Docs pages should reference media through:

- `/assets/static/...` for curated assets.
- `/assets/generated/...` for generated screenshots.

Do not commit image media to `main`.

## Local Development

Install the docs toolchain:

```bash
make docs-install
```

Hydrate assets and build:

```bash
AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1 make docs-build
```

Preview the docs tier with live reload (absolute `/docs/` and marketing links do
not resolve in this mode):

```bash
AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1 make docs-serve
```

Preview the full assembled site (marketing tier, `/docs/`, Markdown twins) after
a build:

```bash
AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1 make docs-build
make docs-preview
```

Run docs validation:

```bash
AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1 make docs-check
```

Without `AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1`, hydration force-fetches
`origin/docs-assets` and `origin/docs-generated-assets` to avoid stale local
asset refs.

## Updating Generated Screenshots

Regenerate screenshots and update the local `docs-generated-assets` orphan
branch:

```bash
make docs-generated-assets-branch
```

Push that branch when generated screenshots should be published:

```bash
bash docs/screenshots/update-generated-assets-branch.sh --push
```

For the initial import or a manual refresh from an existing directory:

```bash
bash docs/screenshots/update-generated-assets-branch.sh \
  --source docs/assets/generated --push
```

## Updating Static Assets

Hydrate or stage curated media under ignored `docs/assets/static/`, then update
the local `docs-assets` orphan branch:

```bash
make docs-assets-branch
```

Push it only when curated static assets should be published:

```bash
bash docs/assets/update-static-assets-branch.sh --push
```

## Publishing

The Vercel project should be linked from the repository root with `docs/` as the
Vercel root directory:

| Setting          | Value                                    |
| ---------------- | ---------------------------------------- |
| Framework preset | `Other`                                  |
| Root directory   | `docs`                                   |
| Install command  | `uv sync --frozen --no-dev`              |
| Build command    | `uv run --frozen bash ./vercel-build.sh` |
| Output directory | `site`                                   |

Link the checkout once from the repository root:

```bash
vercel link
```

Deploy committed docs changes with:

```bash
scripts/update-docs.sh
```

Create a Vercel preview/staging deployment before production with:

```bash
make docs-deploy-staging
```

Use `make docs-deploy` directly only when the asset branches and local build
state are already correct.
