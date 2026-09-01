# Release Documentation Refresh Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish an accurate, outcome-first 0.42.0 story across the changelog,
Zensical documentation, marketing site, and generated screenshots.

**Architecture:** Keep the existing three-tier site and asset-branch design.
Edit the canonical Markdown and hand-written marketing sources, regenerate the
privacy-filtered UI screenshots on the orphan asset branch, assemble the site,
and inspect desktop and mobile renders.

**Tech Stack:** Markdown, Zensical, static HTML/CSS/JavaScript, Go documentation
validators, Docker, Playwright, Git orphan asset branches.

## Global Constraints

- Keep the current site routes, visual system, and nine-part product story.
- Lead with user outcomes. Put implementation detail after the visible change.
- Use “local-first system of record” in the marketing hero subline, not the
  headline.
- Describe AgentsView as local by default. Explain that data leaves the machine
  only when the user configures a remote or hosted target.
- Preserve changelog dates, facts, limits, issue references, and
  acknowledgements from 0.36.0 through 0.42.0.
- Keep image media off `main`; generated PNGs belong on `docs-generated-assets`.
- Do not push branches, deploy the site, or publish assets.

______________________________________________________________________

### Task 1: Set the Changelog Standard and Rewrite Recent Releases

**Files:**

- Modify: `AGENTS.md`
- Modify: `docs/changelog.md`

**Interfaces:**

- Consumes: the 0.42.0 feature list and release tags from `v0.36.0` through
  `v0.42.0`.

- Produces: the durable writing rule and canonical public release history used
  by Zensical and the Markdown twin.

- [ ] **Step 1: Add the durable changelog rule**

    Add a `Changelog` subsection under `Content and Publishing` in `AGENTS.md`.
    Require plain language, user-visible outcomes first, and technical details
    only when they help readers act or understand a limit.

- [ ] **Step 2: Promote 0.42.0 from Unreleased**

    Replace `## Unreleased` with `## 0.42.0` dated `2026-09-01`. Rewrite the
    supplied features, improvements, and fixes as outcome-first bullets.
    Preserve exact command and config names where readers need them.

- [ ] **Step 3: Rewrite the previous two months**

    Review every release from 0.41.1 through 0.36.0. Rewrite bullets that begin
    with internal mechanisms, compress implementation detail that does not
    change user action, define specialized terms on first use, and retain each
    release's acknowledgements unchanged except for line wrapping.

- [ ] **Step 4: Format and inspect the release history**

    Run:

    ```bash
    docs/.venv/bin/mdformat --wrap 80 AGENTS.md docs/changelog.md
    git diff --check
    sed -n '1,1100p' docs/changelog.md
    ```

    Expected: formatting and whitespace checks pass; versions 0.42.0 through
    0.36.0 remain ordered newest-first with dates and acknowledgements.

### Task 2: Bring the Public Documentation Up to 0.42.0

**Files:**

- Modify: `docs/index.md`
- Modify: `docs/quickstart.md`
- Modify: `docs/commands.md`
- Modify: `docs/configuration.md`
- Modify: `docs/hosted-raw-sync.md`
- Modify: `docs/one-shot-capture.md`
- Modify: `docs/token-usage.md`
- Modify: `docs/session-api.md`
- Modify: `docs/recall.md`
- Modify: `docs/pg-sync.md`
- Modify: `docs/zensical.toml`
- Modify other public `docs/*.md` pages only when the 0.42.0 audit finds an
  inaccurate statement or broken cross-link.

**Interfaces:**

- Consumes: the implemented CLI help, configuration schema, API routes, and
  focused internal design documentation for each 0.42.0 feature.

- Produces: task-oriented documentation and navigation that agree with the
  shipped commands and privacy boundaries.

- [ ] **Step 1: Verify each released contract in source**

    Trace hosted raw sync, capture, S3 provider coverage, source-path opt-in,
    historical pricing, and Recall candidate policy to their owning commands,
    configuration, and API implementation. Record only behavior supported by
    source or executable help.

- [ ] **Step 2: Rewrite hosted raw sync as a released workflow**

    Remove “in development” and “no supported command” language. Lead with the
    outcome: a device can continuously upload recoverable raw session history to
    a configured host. Document device authentication, consent for shortened
    uploads, resumable transfers, checkpoints, status, watching, HTTPS, and the
    separation from parsed PostgreSQL sync.

- [ ] **Step 3: Update the remaining 0.42.0 task pages**

    Make one-shot capture start with its exact per-run usage result. Document the
    provider, S3, Japanese localization, historical pricing, source-path,
    candidate-policy, and usage-cache changes where users configure or observe
    them. Keep experimental or opt-in labels where they remain true.

- [ ] **Step 4: Correct site-wide privacy and navigation language**

    Replace absolute “no cloud services” claims with “local by default” wording.
    Rename the Zensical navigation item to `Hosted Raw Sync`. Add or repair
    links from overview and quick-start pages so readers can find the released
    workflows.

- [ ] **Step 5: Format and validate source Markdown**

    Run:

    ```bash
    docs/.venv/bin/mdformat --wrap 80 docs/*.md
    python3 docs/scripts/check_markdown_sources.py
    git diff --check
    ```

    Expected: all three commands exit successfully.

### Task 3: Make the Marketing Site Lead With the Outcome

**Files:**

- Modify: `docs/website/index.html`
- Modify: `docs/website/index.md`
- Modify: `docs/website/guide/index.html`
- Modify: `docs/website/guide.md`
- Modify: `docs/website/styles/site.css` only if the approved hero copy exposes
  a verified layout problem.

**Interfaces:**

- Consumes: the corrected public documentation and generated screenshot paths.

- Produces: matching human-readable HTML and machine-readable Markdown product
  stories.

- [ ] **Step 1: Rewrite the hero hierarchy**

    Use the outcome-first headline
    `Find every coding-agent session, answer, and cost in one place.` Put
    `AgentsView is the local-first system of record for your coding-agent sessions.`
    at the start of the supporting copy. Keep the install command and primary
    calls to action.

- [ ] **Step 2: Update the nine-part story**

    Preserve the numbered sections and layout. Shorten internal language, add the
    0.42.0 hosted-sync outcome to the sharing story, and make the ownership
    section say local by default rather than claiming no hosted service exists.

- [ ] **Step 3: Keep HTML and Markdown twins equivalent**

    Compare each marketing heading, outcome, command, limitation, and destination
    link between `index.html` and `index.md`, then between the guide HTML and
    Markdown files.

- [ ] **Step 4: Build and inspect the source diff**

    Run:

    ```bash
    docs/.venv/bin/mdformat --wrap 80 docs/website/index.md docs/website/guide.md
    AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1 make docs-build
    git diff --check
    ```

    Expected: the assembled three-tier site builds without validator errors.

### Task 4: Regenerate and Review Published Screenshots

**Files:**

- Regenerate ignored files under: `docs/assets/generated/screenshots/`
- Update local orphan branch: `docs-generated-assets`
- Modify: `docs/screenshots/tests/screenshots.spec.ts` only if a released UI
  route or control makes an existing capture invalid.

**Interfaces:**

- Consumes: the current branch build and the screenshot pipeline's
  privacy-filtered database extractor.

- Produces: the complete expected PNG set used by the marketing site, guide,
  README, and Zensical docs.

- [ ] **Step 1: Run the isolated screenshot pipeline**

    Run:

    ```bash
    make docs-generated-assets-branch
    ```

    Expected: Docker builds the branch binary, Playwright finishes with no failed
    screenshot tests, every expected PNG exists, and the command updates only
    the local `docs-generated-assets` ref.

- [ ] **Step 2: Scan the outgoing media**

    Use the configured private-terms file and structural path, hostname, email,
    and identity checks against the generated file names and extracted image
    text. Inspect PNG metadata and require no unexpected author, path, host, or
    location fields.

- [ ] **Step 3: Inspect every screenshot at original resolution**

    Build contact sheets for coverage, then open any dense, clipped, blank, or
    suspicious image individually at original resolution. Confirm that captions,
    selected controls, tooltips, tables, and modal bounds are visible and that
    no private data appears in pixels.

- [ ] **Step 4: Rehydrate from the local asset branches**

    Run:

    ```bash
    AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1 docs/assets/hydrate-assets.sh
    ```

    Expected: all generated assets hydrate from the new local orphan commit.

### Task 5: Verify the Complete Site and Commit the Refresh

**Files:**

- Verify all files changed by Tasks 1–4.

**Interfaces:**

- Consumes: the edited sources and regenerated asset branch.

- Produces: a committed, reviewable documentation refresh with local rendered
  evidence.

- [ ] **Step 1: Run the complete documentation checks**

    Run:

    ```bash
    AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1 make docs-check
    git diff --check
    ```

    Expected: all source, build, redirect, asset, and link validators pass.

- [ ] **Step 2: Inspect assembled desktop and mobile pages**

    Serve `docs/site/` on loopback. Capture the landing page, guide, docs home,
    hosted raw sync, changelog, and one-shot capture at `1440x900` and
    `390x844`. View every capture and confirm readable hierarchy, no overflow,
    correct image loading, and matching privacy language.

- [ ] **Step 3: Perform the final private-data scan**

    Scan the full tracked diff, untracked files selected for commit, commit
    message, and regenerated screenshot set. Require zero denylist or structural
    privacy hits before committing.

- [ ] **Step 4: Review and commit the tracked changes**

    Review `git status --short`, `git diff --stat`, `git diff HEAD`, and recent
    commit style. Stage only the documentation refresh, run staged whitespace
    and privacy checks, then create one conventional `docs:` commit with a plain
    language body explaining why the release story changed.

### Task 6: Finish Release Monitoring

**Files:** none.

**Interfaces:**

- Consumes: GitHub Actions run IDs `33550358918`, `33550359123`, and
  `33550358969` plus the published `v0.42.0` release record.

- Produces: an evidence-backed final status for release binaries, desktop
  bundles, Docker images, and published assets.

- [ ] **Step 1: Wait for all release workflows to finish**

    Query each run until `status=completed`. If a conclusion is not `success`,
    inspect the failed job and logs before reporting it.

- [ ] **Step 2: Verify the published release record**

    Run:

    ```bash
    gh release view v0.42.0 --json tagName,publishedAt,assets,url
    ```

    Expected: the release exists and lists the artifacts produced by the release
    workflows.
