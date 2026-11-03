# Generate documentation screenshots

Run documentation captures on a managed worker. Each job takes a named native
session corpus, retained build dependencies, and pinned source and image
revisions. It rebuilds the application, imports the corpus, captures every view,
and builds a website preview twice. A successful receipt requires identical
application builds, PNG files, browser versions, and generated-asset commits.

This repository supplies the worker commands. A separate operator service is
still needed for team submission, private storage, access control, worker
installation, and retained results. Keep corpus files, preparation bundles,
logs, and receipts in private managed storage. Connect the commands to that
service before offering routine capture to teammates.

## Prepare the worker inputs

Corpus generation is a separate operation. Follow [corpus.md](corpus.md) to
retain an explicit selection, sanitization recipe, reference time, and scenario
targets. Review a generation before approving it for publication. Normal capture
never reads the original archive, donor files, home directories, or private term
lists.

Prepare the builder and runtime environments on the worker:

```bash
docker build --platform linux/amd64 --target builder \
  --iidfile /managed/images/builder.id docs/screenshots
docker build --platform linux/amd64 --target runtime \
  --iidfile /managed/images/runtime.id docs/screenshots
```

Retain both images and their immutable digests. The Dockerfile pins base images
and the operating-system package snapshot. It installs the locked Playwright
package and matching browser. Environment preparation has network access;
application building, capture, and website preview do not.

From a clean, committed capture checkout, retain the selected application's
source, dependencies, pricing snapshot, documentation dependencies, and static
asset objects:

```bash
python3 docs/screenshots/prepare.py prepare \
  --source /managed/source/agentsview \
  --revision "$APPLICATION_REVISION" \
  --version "$APPLICATION_VERSION" \
  --build-date "$BUILD_DATE" \
  --static-assets-revision "$STATIC_ASSETS_REVISION" \
  --builder-image "$BUILDER_IMAGE" \
  --runtime-image "$RUNTIME_IMAGE" \
  --output /managed/prepared/release-1
```

Use full Git commit IDs, an explicit UTC build date, and image references of the
form `sha256:...` or `registry/image@sha256:...`. `AGENTSVIEW_SRC` supplies the
application checkout when `--source` is omitted. Documentation comes from the
application revision; the preparation-tool checkout is recorded separately.
Capture jobs pin their own script revision, so a capture-only change can reuse
the same retained dependencies. Keep both source revisions available for
retries.

The output directory must be new. The command prints the SHA-256 of
`prepared.json`. Retain the entire directory and record that digest with the
request. Later runs verify every dependency file and symlink against this sealed
manifest. Updating dependencies or source creates a new preparation, with a new
digest; retries reuse the existing preparation.

## Run a capture job

The worker sets explicit inputs and invokes the existing entry point:

```bash
CORPUS_MANIFEST=/managed/corpora/example-1/manifest.json \
CORPUS_SHA256="$APPROVED_CORPUS_SHA256" \
PREPARED_INPUTS=/managed/prepared/release-1 \
PREPARED_SHA256="$SEALED_PREPARATION_SHA256" \
CAPTURE_REVISION="$CAPTURE_SCRIPT_REVISION" \
CAPTURE_OUTPUT=/managed/attempts/request-1-attempt-1 \
  make docs-screenshots
```

Use a new output directory for every attempt. The job rejects existing output
and never borrows images from an earlier run. A failure keeps its logs and
partial results, without writing the job's success receipt. Retry with the same
sealed inputs and a new directory. Interrupting a worker removes its active
container.

Each of the two runs:

1. Builds the frontend and Go binary from retained dependencies with networking
   disabled and fixed build metadata.
1. Copies the corpus into the attempt and fixes file and directory modification
   times to its reference date, so restoring files cannot change duplicate
   transcript selection. Creates an empty home and fresh data directories. It
   queries the selected application's provider registry, configures only the
   corpus roots, and runs normal sync. Other configurable provider roots are
   empty.
1. Checks the required sessions and content, then applies the corpus's reviewed
   stars and insight examples. PostgreSQL shows two example machines, with a
   stable timestamp-and-session-ID split.
1. Captures all cases with the recorded clock and timezone. Chromium, locale,
   fonts, scale, and architecture come from pinned inputs. Progress fixtures
   do not depend on poll counts or host time.
1. Checks the exact PNG inventory in `expected-assets.txt`, creates a local
   orphan asset commit, and builds the assembled website offline. Both Git
   dates and identities are fixed. Both asset refs exist locally; no hydrator
   fetch is required. Missing documentation tools fail the attempt.

Preview publication and hydration use the pinned capture revision's tools and
screenshot inventory. Documentation content and its dependencies come from the
prepared application revision, so adding a capture does not require preparing
the same application again.

`receipt.json` identifies the inputs and both results. Its `publication_assets`
and `preview` fields locate the final images and assembled website. The receipt
includes site file hashes for inspection; site bytes are not part of the capture
comparison. Logs and intermediate databases are diagnostic data, not equality
inputs. Retain failed-attempt logs as well as successful results.

## Review and publish

Inspect the full screenshot set and open the assembled preview, including the
homepage and affected guides. Check populated content, readable controls, and
private data. A successful hash comparison does not replace this review.

After publication is authorized, import the reviewed job's asset directory:

```bash
bash docs/screenshots/update-generated-assets-branch.sh \
  --source /managed/attempts/request-1-attempt-1/run-1/capture/assets
```

To reproduce the receipt's asset commit, run the import with `GIT_AUTHOR_DATE`
and `GIT_COMMITTER_DATE` set to its `reference_now`, both Git names set to
`Documentation Capture`, and both Git emails set to `docs@example.invalid`. The
preview step supplies these values itself.

This updates the local `docs-generated-assets` orphan branch. Publication
remains a separate action; add `--push` only when authorized. Keep corpus and
receipt files out of this branch. See the [maintainer guide](../README.md) for
website publication.

## Update the capture cases

Add cases in `tests/screenshots.spec.ts` and filenames in `expected-assets.txt`.
The runner, publisher, and hydrator share that inventory. Add native examples or
explicit reviewed response fixtures when a feature needs new coverage. Missing
content must fail; it must not silently leave an old PNG in the result.

Most captures use a 1440×900 viewport at scale 1, dark mode, and English. The
corpus supplies the timezone and reference time. One worker runs cases in order.
Activity captures use the completed previous week so the server's live cutoff
cannot move between runs. The browser disables partial rasterization to avoid
different edge pixels after identical page loads. Captures hide session
identifiers and refresh timing labels; measured request latency varies between
machines even with a fixed browser clock. Response fixtures illustrate
unavailable integrations and selected analysis states. Capture does not invoke
models, contact GitHub, or start cloud sync.

Run focused pipeline tests before preparing a new capture revision:

```bash
python3 -m unittest discover -s docs/screenshots -p 'test_*.py'
```

Use the worker's complete job for acceptance. A single capture or a successful
application build does not establish reproducibility.
