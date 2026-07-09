# Command Palette Semantic Search Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development
> (if subagents available) or superpowers:executing-plans to implement this
> plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add remembered Full text, Semantic, and Hybrid modes to the existing
command palette without changing backend contracts or exposing raw semantic
snippets to an HTML sink.

**Architecture:** Extend the existing search store into the single mode-aware
request owner. It will normalize the session-search and content-search response
shapes into a palette-specific result model, while the command palette renders
that model through kit-ui's `SegmentedControl` and mode-aware snippet handling.
The backend APIs remain unchanged.

**Tech Stack:** Svelte 5 runes, TypeScript, Vite+/Vitest, Paraglide JS,
`@kenn-io/kit-ui`, generated OpenAPI client.

______________________________________________________________________

## File Structure

- `frontend/src/lib/stores/search.svelte.ts`: own the remembered search mode,
  mode-aware request dispatch, normalized palette results, deduplication,
  cancellation, and request error state.
- `frontend/src/lib/stores/search.test.ts`: protect store behavior at the
  generated-service and storage boundaries.
- `frontend/src/lib/components/command-palette/CommandPalette.svelte`: render
  the kit-ui mode control, conditional full-text sorting, safe snippets, and
  actionable error state.
- `frontend/src/lib/components/command-palette/CommandPalette.test.ts`: exercise
  the real component and real kit-ui control through rendered DOM behavior.
- `frontend/messages/{en,zh-CN,zh-TW,ko}.json`: keep the four locale catalogs in
  sync for mode and error labels.
- `docs/usage.md`: describe the three command-palette modes and remembered
  selection.
- `docs/semantic-search.md`: replace the no-frontend limitation with the
  command-palette integration and configuration dependency.

## Baseline Note

`npm run check` and `npm test` passed before implementation (0 Svelte
diagnostics; 117 files and 1,788 tests passed). `npx vp check` has a
pre-existing repository-wide formatter failure over 420 files, including
generated API code; do not apply that unrelated mass rewrite. Run focused
formatting/checks for changed files plus the established full type and test
gates.

### Task 1: Mode-aware store and localized command-palette UI

**Files:**

- Modify: `frontend/src/lib/stores/search.test.ts`
- Modify: `frontend/src/lib/stores/search.svelte.ts`
- Modify: `frontend/src/lib/components/command-palette/CommandPalette.test.ts`
- Modify: `frontend/src/lib/components/command-palette/CommandPalette.svelte`
- Modify: `frontend/messages/en.json`
- Modify: `frontend/messages/zh-CN.json`
- Modify: `frontend/messages/zh-TW.json`
- Modify: `frontend/messages/ko.json`

**Required skills:** @superpowers:test-driven-development,
@testing-without-tautologies, @localization-paraglide

The normalized result contract changes both producer and consumer. Complete all
store and component phases below before committing so no intermediate commit
leaves `CommandPalette.svelte` incompatible with `searchStore.results`.

- [ ] **Step 1: Add failing tests for mode persistence and request routing**

    Extend the generated service mock with `getApiV1SearchContent`. Add concrete
    tests proving:

    - absent or invalid storage defaults to `fulltext`;
    - a valid stored `semantic` or `hybrid` mode is restored;
    - `setMode()` writes `agentsview-search-mode` and preserves the selected mode
      across `clear()`;
    - Full text calls `getApiV1Search` with `limit: 30` and the active sort; and
    - a whitespace-padded Semantic/Hybrid query calls `getApiV1SearchContent` with
      a trimmed pattern, exact mode, project, `limit: 120`, and
      `xAgentsViewSearchIntent: "semantic"`.

    Assert complete argument objects for both services, including `q` and project
    on Full text. Do not use `expect.objectContaining` for these contract tests.

    Instantiate an exported `SearchStore` or store factory with an isolated
    `Storage` boundary so persistence tests do not depend on singleton import
    timing.

- [ ] **Step 2: Run the focused store test and confirm RED**

    Run:

    ```bash
    cd frontend
    npx vp test run src/lib/stores/search.test.ts
    ```

    Expected: failures because `SearchMode`, `setMode`, storage restoration, and
    the content-search request path do not exist.

- [ ] **Step 3: Implement mode state, persistence, and dispatch minimally**

    Add view-specific types and constants in the store rather than broadening the
    backend API types:

    ```ts
    export type SearchMode = "fulltext" | "semantic" | "hybrid";

    export interface PaletteSearchResult {
      session_id: string;
      project: string;
      agent: string;
      name?: string;
      ordinal: number;
      timestamp: string;
      snippet: string;
      rank: number;
      snippetFormat: "highlighted-html" | "plain-text";
    }

    const SEARCH_MODE_KEY = "agentsview-search-mode";
    const RESULT_LIMIT = 30;
    const CONTENT_CANDIDATE_LIMIT = 120;
    ```

    Export the class or a factory for isolated tests, while retaining the existing
    singleton export for the app. Read storage defensively and persist only
    valid modes. `setMode()` must cancel queued and in-flight work, persist the
    new mode, and immediately rerun an active query.

    Route generated requests through `callGenerated()` so structured 501/503
    bodies become actionable `ApiError.message` values. Continue using
    `isAbortError()` to make cancellation invisible.

- [ ] **Step 4: Add failing tests for normalization, errors, and stale work**

    Add literal `DbContentMatch` fixtures proving:

    - ranked matches keep the first match for each session and truncate to 30
      unique sessions;
    - content results map `timestamp` and `snippetFormat: "plain-text"`, while
      Full text maps `session_ended_at` and `snippetFormat: "highlighted-html"`;
    - a mode change cancels an older request and only the newest request mutates
      results/loading/error;
    - realistic generated 501 and 503 errors preserve the `{ error: ... }` body
      detail;
    - a success clears the previous error; and
    - `clear()` clears visible errors without resetting mode or sort.

    Use the real `callGenerated()` runtime helper and real generated `ApiError`
    instances containing structured `{ error: ... }` bodies. Mock only the
    generated service boundary; do not keep the current passthrough
    `callGenerated` mock. Use separate success, cancellation, 501, and 503
    doubles. Assert exact request arguments and observable store state; do not
    mirror the production mapper in test helpers.

- [ ] **Step 5: Run the focused store test and confirm RED**

    Run the same focused command. Expected: the new normalization and error
    lifecycle assertions fail for missing behavior, not fixture or import
    errors.

- [ ] **Step 6: Implement normalization and error lifecycle minimally**

    Normalize full-text and content responses in separate focused functions.
    Deduplicate content matches in backend rank order with a `Set<string>`, stop
    at 30 unique sessions, and never resort them. Set `error = null` when a
    request starts or succeeds; on a non-abort failure preserve the previous
    results, store the actionable message, and stop loading. Guard every final
    state write with the active request's non-aborted signal.

- [ ] **Step 7: Run the store tests**

    ```bash
    cd frontend
    npx vp test run src/lib/stores/search.test.ts
    ```

    Expected: focused store tests pass. Do not run or claim the full type checker
    yet; the consumer contract is migrated in the next phase of this same task.

- [ ] **Step 8: Self-review the store phase without committing**

    Review every new assertion against the mutation question: wrong endpoint,
    wrong intent header, no over-fetch, no deduplication, stale request write,
    or generic HTTP status must each break at least one test.

    Continue directly into the consumer phase so the eventual commit is
    type-correct and atomic.

#### Consumer phase: kit-ui mode control and safe result rendering

- [ ] **Step 9: Expand the search-store component double**

    Give the existing hoisted store double concrete `mode`, `error`, `setMode`,
    and normalized result fields. Reset all values in `beforeEach` so tests
    cannot leak mode or error state.

- [ ] **Step 10: Add failing DOM tests for the real mode control and sort
  state**

    Mount `CommandPalette` and assert observable behavior:

    - a radiogroup with the localized search-mode label contains Full text,
      Semantic, and Hybrid radio buttons;
    - selecting Semantic calls `setMode("semantic")` and resets the selected
      result index;
    - Relevance/Recency is visible only when results are showing in Full text;
    - Enter/Space/Left/Right in the mode control and keyboard activation of sort
      buttons never call result navigation; and
    - Escape from the mode or sort control still closes the palette.

    Exercise the rendered kit-ui control through roles and keyboard events. Do not
    test kit-ui's private classes or re-prove its internal roving-focus
    algorithm.

- [ ] **Step 11: Run the focused component test and confirm RED**

    ```bash
    cd frontend
    npx vp test run src/lib/components/command-palette/CommandPalette.test.ts
    ```

    Expected: failures because the radiogroup, mode options, conditional sort
    behavior, and control-row keyboard isolation are absent.

- [ ] **Step 12: Add synchronized localized messages**

    Add identical keys to all four catalogs:

    ```json
    "command_palette_search_mode_label": "Search mode",
    "command_palette_mode_fulltext": "Full text",
    "command_palette_mode_semantic": "Semantic",
    "command_palette_mode_hybrid": "Hybrid",
    "command_palette_search_error": "Search unavailable"
    ```

    Use accurate Simplified Chinese, Traditional Chinese, and Korean translations;
    keep technical backend remediation text verbatim.

- [ ] **Step 13: Implement the mode row with kit-ui `SegmentedControl`**

    Import `SegmentedControl` and its option type from `@kenn-io/kit-ui`. Build
    the translated options in `$derived` state so locale reloads cannot leave
    stale labels. Render the mode control in a `.palette-controls` row below the
    input. Do not create mode buttons or mode-specific control chrome.

    Keep the current sort buttons on the right only when
    `showSearchResults && searchStore.mode === "fulltext"`. In the overlay
    keydown handler, events from `.palette-controls` return early for every key
    except Escape, allowing the palette-wide Escape close path to remain active.

- [ ] **Step 14: Add failing DOM tests for state precedence and snippet safety**

    Add concrete results and assert:

    - loading takes precedence over a stale error or stale results;
    - an error takes precedence over empty/results once loading stops;
    - an empty successful response renders the normal no-results state;
    - a store error renders the localized error heading plus exact backend detail;
    - a semantic snippet such as `<img src=x onerror=alert(1)>` appears as literal
      text and creates no `img` element;
    - a Full text snippet containing `<mark>needle</mark>` still renders a mark;
      and
    - clicking either normalized result navigates and scrolls to its ordinal.

- [ ] **Step 15: Run the focused component test and confirm RED**

    Expected: error and plain-text snippet assertions fail before the rendering
    branch is added.

- [ ] **Step 16: Implement state precedence and mode-aware snippet rendering**

    Render loading first, then the error state, then empty/results. For
    `snippetFormat === "highlighted-html"`, retain
    `{@html sanitizeSnippet(result.snippet)}`. For `plain-text`, render with
    normal Svelte interpolation only. Use the normalized `timestamp` in
    metadata.

- [ ] **Step 17: Compile localization and verify the complete atomic change**

    ```bash
    cd frontend
    npm run i18n:compile
    npx vp test run src/lib/stores/search.test.ts
    npx vp test run src/lib/components/command-palette/CommandPalette.test.ts
    npm run check
    npm run check:kit-ui
    ```

    Expected: Paraglide compilation succeeds, focused tests pass, Svelte reports
    no diagnostics, and kit-ui policy checks report no violations.

- [ ] **Step 18: Self-review and commit the atomic producer/consumer change**

    Confirm the locale key sets are identical and the component imports the shared
    control directly. Follow @kenn:commit and repository `AGENTS.md`.

    ```bash
    git add frontend/messages/*.json \
      frontend/src/lib/stores/search.svelte.ts \
      frontend/src/lib/stores/search.test.ts \
      frontend/src/lib/components/command-palette/CommandPalette.svelte \
      frontend/src/lib/components/command-palette/CommandPalette.test.ts
    git commit -m "feat(search): add semantic command palette modes"
    ```

### Task 2: User documentation and final verification

**Files:**

- Modify: `docs/usage.md`
- Modify: `docs/semantic-search.md`

**Required skills:** @kenn:verify-before-handoff,
@superpowers:verification-before-completion, @browser:control-in-app-browser

- [ ] **Step 1: Update user documentation**

    In `docs/usage.md`, replace the Full-Text-only command-palette description
    with the three modes, remembered selection, Full text-only sort, and the
    fact that Semantic/Hybrid require configured embeddings and an active index.

    In `docs/semantic-search.md`, replace the “No frontend integration” limitation
    with a concise Command Palette subsection that explains
    one-result-per-session presentation, ranked Semantic/Hybrid behavior, and
    configuration through the CLI/config rather than the browser.

- [ ] **Step 2: Format and inspect documentation changes**

    ```bash
    mdformat --wrap 80 docs/usage.md docs/semantic-search.md
    git diff --check
    ```

    Expected: no whitespace errors and no unrelated documentation churn.

- [ ] **Step 3: Run full frontend verification fresh**

    ```bash
    cd frontend
    npm run i18n:compile
    npm run check
    npm run check:kit-ui
    npm test
    npm run build
    ```

    Expected: localization, type checking, policy checks, all frontend tests, and
    the production build pass. Also run a focused formatter/check on only
    changed frontend files if supported; do not mass-format the 420-file
    baseline.

- [ ] **Step 4: Run the branch app through the isolated E2E server**

    Do not use the live daemon or `~/.agentsview`. Launch `scripts/e2e-server.sh`,
    which builds the branch, seeds a temporary database, derives every parser
    `EnvVar` from `internal/parser/types.go`, points all of those agent
    directories at an empty temporary directory, and serves on port 8090.
    Confirm the served asset is from the current branch before inspecting UI
    behavior. Do not replace this with `AGENTSVIEW_DATA_DIR` alone because that
    does not isolate parser discovery from live agent directories.

- [ ] **Step 5: Verify the real command palette in the browser**

    Open the scratch app, invoke the command palette, and observe:

    - all three localized mode options render through the segmented control;
    - the selected mode survives close/reopen and a hard reload;
    - Full text retains Relevance/Recency while Semantic/Hybrid hide it;
    - a Semantic query against the intentionally unconfigured scratch index shows
      the actionable setup detail without mode fallback;
    - keyboard mode selection never opens a result and Escape closes; and
    - a Full text result still opens the correct session and ordinal.

    Capture DOM/screenshot evidence. If the in-app browser is unavailable, use the
    repository Playwright surface against the same scratch server; do not treat
    a skipped live check as success.

- [ ] **Step 6: Review the complete diff against the approved spec**

    Re-read `docs/superpowers/specs/2026-07-09-ui-semantic-search-design.md`.
    Verify every goal, non-goal, error rule, localization rule, and safety rule
    against the diff and observed behavior.

- [ ] **Step 7: Commit documentation and any verification-only fixture changes**

    Follow @kenn:commit and repository `AGENTS.md`. Do not commit scratch
    databases, screenshots containing private data, generated browser state, or
    ignored visual-companion artifacts.

    ```bash
    git add docs/usage.md docs/semantic-search.md
    git commit -m "docs: document command palette semantic search"
    ```
