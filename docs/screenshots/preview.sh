#!/usr/bin/env bash
set -euo pipefail
cd /src
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/tmp/preview-gitconfig
export GIT_AUTHOR_NAME='Documentation Capture' GIT_AUTHOR_EMAIL='docs@example.invalid'
export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME" GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"
export GIT_AUTHOR_DATE="$REFERENCE_NOW" GIT_COMMITTER_DATE="$REFERENCE_NOW"
export AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1
command -v uv > /dev/null
git init --quiet
git index-pack --stdin < /bundle/static-assets.pack > /dev/null
git update-ref refs/heads/docs-assets "$STATIC_ASSETS_REVISION"
bash docs/screenshots/update-generated-assets-branch.sh --source /screenshots
git rev-parse docs-generated-assets > /out/generated-asset-commit.txt
cp -a /bundle/docs_venv docs/.venv
python3 docs/scripts/check_markdown_sources.py
bash docs/assets/hydrate-assets.sh
(cd docs && uv run --offline --frozen --no-dev bash ./zensical-docs.sh build && \
  uv run --offline --frozen --no-dev python scripts/check_built_site.py && \
  uv run --offline --frozen --no-dev python scripts/check_vercel_redirects.py)
cp -a docs/site /out/site
