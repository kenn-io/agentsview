#!/usr/bin/env bash
# Capture the checked-out commit with an approved corpus and write the complete
# docs-generated-assets tree to OUTPUT/assets and the built website to
# OUTPUT/preview. The host needs Bash, Git, Python 3 and Docker.
#
# Inputs (environment):
#   DOCS_SCREENSHOTS_OUTPUT    new or empty results directory
#   DOCS_SCREENSHOTS_CORPUS    approved corpus manifest.json
#   DOCS_SCREENSHOTS_REVISION  full commit ID of the checkout (default: HEAD)
#   DOCS_SCREENSHOTS_DATE      UTC build date, e.g. 2026-04-01T12:00:00Z
#                              (default: the corpus reference time)
set -euo pipefail

output="${DOCS_SCREENSHOTS_OUTPUT:?DOCS_SCREENSHOTS_OUTPUT is required}"
corpus="${DOCS_SCREENSHOTS_CORPUS:?DOCS_SCREENSHOTS_CORPUS is required}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
revision="${DOCS_SCREENSHOTS_REVISION:-$(git -C "$repo_root" rev-parse HEAD)}"
build_date="${DOCS_SCREENSHOTS_DATE:-$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["reference_now"])' "$corpus")}"
repository="$(git -C "$repo_root" remote get-url origin 2>/dev/null || echo https://github.com/kenn-io/agentsview.git)"

mkdir -p "$output"
output="$(cd "$output" && pwd -P)"
corpus="$(cd "$(dirname "$corpus")" && pwd -P)/$(basename "$corpus")"

# Retained dependencies are large, so they stay out of the results directory.
# The checkout is visible to the Docker daemon, unlike a private /tmp.
work="$(mktemp -d "$repo_root/.docs-capture.XXXXXX")"
cleanup() {
  status=$?
  mkdir -p "$output/logs"
  find "$work" -path "$work/prepared/bundle" -prune -o -name '*.log' -type f -print | while IFS= read -r log; do
    name="${log#"$work"/}"
    cp "$log" "$output/logs/${name//\//-}"
  done
  [[ -f "$work/job/receipt.json" ]] && cp "$work/job/receipt.json" "$output/logs/capture-job.json"
  # The Go module cache is read-only; make it removable first.
  chmod -R u+w "$work" 2>/dev/null || true
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT

docker build --quiet --platform linux/amd64 --target builder \
  --iidfile "$work/builder.id" "$script_dir" >/dev/null
docker build --quiet --platform linux/amd64 --target runtime \
  --iidfile "$work/runtime.id" "$script_dir" >/dev/null

git -C "$repo_root" fetch --quiet --depth=1 "$repository" refs/heads/docs-assets
static_assets_revision="$(git -C "$repo_root" rev-parse FETCH_HEAD)"

prepared_sha256="$(python3 "$script_dir/prepare.py" prepare \
  --source "$repo_root" \
  --revision "$revision" \
  --version "$(git -C "$repo_root" describe --tags --always)" \
  --build-date "$build_date" \
  --static-assets-revision "$static_assets_revision" \
  --builder-image "$(cat "$work/builder.id")" \
  --runtime-image "$(cat "$work/runtime.id")" \
  --output "$work/prepared")"

CORPUS_MANIFEST="$corpus" \
CORPUS_SHA256="$(python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())' "$corpus")" \
PREPARED_INPUTS="$work/prepared" \
PREPARED_SHA256="$prepared_sha256" \
CAPTURE_REVISION="$revision" \
CAPTURE_OUTPUT="$work/job" \
  python3 "$script_dir/job.py" >/dev/null

mkdir -p "$output/assets" "$output/preview"
cp -R "$work/job/run-1/capture/assets/." "$output/assets/"
cp -R "$work/job/run-1/preview/site/." "$output/preview/"
