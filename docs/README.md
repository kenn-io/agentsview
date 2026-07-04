# AgentsView docs maintainer guide

This directory contains the Zensical source for <https://agentsview.io>. The
docs source lives on `main`; image media lives on orphan asset branches so
normal clones do not pull screenshots and PNGs into the main history.

## Layout

- `*.md` and related public subdirectories: public docs source.
- `internal/`: maintainer references excluded from the published site.
- `zensical.toml`: Zensical site configuration and navigation.
- `pyproject.toml` and `uv.lock`: pinned docs toolchain.
- `vercel.json` and `vercel-build.sh`: Vercel project configuration.
- `zensical-docs.sh`: builds from a temporary public-docs copy so maintainer
  files are excluded from the published site.
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

Preview locally:

```bash
AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1 make docs-serve
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
