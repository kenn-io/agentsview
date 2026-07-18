# Kit UI Card and Checkbox Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pin the current kit-ui release and eliminate all 37 hand-rolled-card
and four hand-rolled-checkbox findings without changing frontend behavior or
layout.

**Architecture:** Import kit-ui components directly at each affected Svelte call
site. Real content surfaces move to `Card`; selection and on/off controls move
to `Checkbox` or `Toggle`; compact controls accidentally matching the card
recipe move to the existing semantically appropriate kit component instead of
being wrapped in card markup. Feature classes retain only app-owned layout and
state styling.

**Tech Stack:** Svelte 5, TypeScript, Vitest, Playwright, Paraglide JS,
`@kenn-io/kit-ui`, and `kit-ui-check`.

## Global Constraints

- Pin `@kenn-io/kit-ui` to commit `a362f4ecfbc6d4381225c771ec423df5b62a8d09`.
- Preserve layout, interactions, accessibility labels, disabled states,
  bindings, and event behavior.
- Do not add user-facing copy or change locale catalogs.
- Do not globally disable `hand-rolled-card` or `hand-rolled-checkbox`.
- Do not add a `kit-ui-check-ignore` marker unless a concrete component gap is
  found and documented beside the marker.
- Avoid unrelated restyling and frontend refactoring.
- Do not add tests that retest library behavior; protect app-owned state
  transitions and rendered contracts.

______________________________________________________________________

### Task 1: Enable the New Kit UI Components and Conformance Rules

**Files:**

- Modify: `frontend/package.json:22`
- Modify: `frontend/package-lock.json`

**Interfaces:**

- Consumes: the public git dependency at the exact commit above.

- Produces: imports for `Card`, `Checkbox`, and `Toggle`, plus the full current
  `kit-ui-check` rule set.

- [ ] **Step 1: Update the dependency and lockfile**

Run from `frontend/`:

```bash
npm install '@kenn-io/kit-ui@git+https://github.com/kenn-io/kit-ui.git#a362f4ecfbc6d4381225c771ec423df5b62a8d09'
```

Expected: `package.json` and `package-lock.json` resolve kit-ui at `a362f4e`.

- [ ] **Step 2: Verify the red conformance state**

Run:

```bash
npm run check:kit-ui
```

Expected: FAIL with exactly 41 findings: 37 `hand-rolled-card` and four
`hand-rolled-checkbox` findings. A different inventory means the dependency or
source tree changed and must be reconciled before migration.

- [ ] **Step 3: Verify the dependency compiles before app migration**

Run:

```bash
npm run check
```

Expected: PASS. The dependency bump alone must not break existing imports.

- [ ] **Step 4: Commit the dependency bump**

```bash
git add frontend/package.json frontend/package-lock.json
git commit -m "chore(frontend): update kit-ui card controls"
```

### Task 2: Migrate the Four Native Checkbox Controls

**Files:**

- Modify: `frontend/src/lib/components/settings/AppearanceSettings.svelte`
- Modify: `frontend/src/lib/components/settings/DateRangeSettings.svelte`
- Modify: `frontend/src/lib/components/settings/WorktreeMappingSettings.svelte`
- Modify: `frontend/src/lib/components/trends/TrendsPage.svelte`
- Test: `frontend/src/lib/components/settings/DateRangeSettings.test.ts`
- Test: `frontend/src/lib/components/trends/TrendsPage.test.ts`

**Interfaces:**

- Consumes: `Checkbox.checked`, `Checkbox.onchange`, `Toggle.checked`, and
  `Toggle.onchange(checked: boolean)`.

- Produces: the same store mutations as the native controls, with `switch`
  semantics for immediate settings and `checkbox` semantics for selections.

- [ ] **Step 1: Add focused semantic assertions before changing markup**

Extend the date-range and trends component tests with the app-owned switch
semantics required for immediate on/off settings:

```ts
const linkedDates = getByRole("switch", {
  name: "Link date ranges across pages",
}) as HTMLInputElement;
expect(linkedDates.checked).toBe(false);

const normalize = document.querySelector<HTMLInputElement>(
  'input[role="switch"][aria-label="Normalize by number of messages"]',
);
expect(normalize).not.toBeNull();
```

Both assertions are expected to fail against the current native checkbox markup
because it has no `role="switch"`. Existing date-range persistence and worktree
save assertions already protect the relevant callbacks; do not add duplicate
tests for kit-ui's checkbox implementation.

- [ ] **Step 2: Run the focused tests and confirm the expected red state**

Run:

```bash
npm test -- src/lib/components/settings/DateRangeSettings.test.ts src/lib/components/trends/TrendsPage.test.ts
```

Expected: only the new switch-semantic assertions fail.

- [ ] **Step 3: Replace the controls with kit components**

Use these call-site contracts:

```svelte
<Checkbox
  checked={ui.isBlockVisible(bt)}
  onchange={() => ui.toggleBlock(bt)}
  label={BLOCK_LABELS[bt]}
/>

<Toggle
  checked={yokedDates.enabled}
  onchange={(checked) => yokedDates.setEnabled(checked)}
  label={m.settings_date_ranges_link()}
/>

<Checkbox bind:checked={enabled} label={m.worktree_enabled()} />

<Toggle
  checked={trends.normalized}
  onchange={(checked) => {
    trends.normalized = checked;
    writeUrl();
  }}
  label={m.trends_normalize()}
/>
```

Import `Checkbox` or `Toggle` from `@kenn-io/kit-ui` in each file. Remove the
native `<label><input type="checkbox">` wrappers and delete only the obsolete
checkbox label/input CSS. Keep surrounding flex/grid layout where it is still
used by other content.

- [ ] **Step 4: Verify the focused tests and conformance count**

Run:

```bash
npm test -- src/lib/components/settings/AppearanceSettings.test.ts src/lib/components/settings/DateRangeSettings.test.ts src/lib/components/settings/WorktreeMappingSettings.test.ts src/lib/components/trends/TrendsPage.test.ts
npm run check:kit-ui
```

Expected: focused tests PASS; conformance now reports 37 card findings and zero
checkbox findings.

- [ ] **Step 5: Commit the boolean-control migration**

```bash
git add frontend/src/lib/components/settings/AppearanceSettings.svelte frontend/src/lib/components/settings/DateRangeSettings.svelte frontend/src/lib/components/settings/DateRangeSettings.test.ts frontend/src/lib/components/settings/WorktreeMappingSettings.svelte frontend/src/lib/components/trends/TrendsPage.svelte frontend/src/lib/components/trends/TrendsPage.test.ts
git commit -m "refactor(frontend): adopt kit boolean controls"
```

### Task 3: Migrate Content Surfaces to Card

**Files:**

- Modify: `frontend/src/lib/components/activity/ActivityPage.svelte`
- Modify: `frontend/src/lib/components/activity/SummaryCards.svelte`
- Modify: `frontend/src/lib/components/analytics/AnalyticsPage.svelte`
- Modify: `frontend/src/lib/components/analytics/SessionHealthSection.svelte`
- Modify: `frontend/src/lib/components/analytics/SummaryCards.svelte`
- Modify: `frontend/src/lib/components/insights/InsightsPage.svelte`
- Modify: `frontend/src/lib/components/settings/WorktreeMappingSettings.svelte`
- Modify: `frontend/src/lib/components/usage/UsagePage.svelte`
- Modify:
  `frontend/src/lib/components/usage/UsagePairwiseComparisonPanel.svelte`
- Modify: `frontend/src/lib/components/usage/UsageSummaryCards.svelte`

**Interfaces:**

- Consumes: `Card level="default" | "inset"`, `padding="none"`, `onclick`,
  `selected`, and `class`.

- Produces: unchanged feature DOM content inside kit-owned surface chrome.

- [ ] **Step 1: Replace default-level surfaces**

Import `Card` and change the root element for each listed selector to:

```svelte
<Card
  level="default"
  padding="none"
  class={card.featured ? "card featured" : "card"}
>
  <span class="card-value">{card.value}</span>
  <span class="card-label">{card.label}</span>
  {#if card.sub}
    <span class="card-sub">{card.sub}</span>
  {/if}
</Card>
```

The summary-card body above is the exact replacement in
`activity/SummaryCards.svelte`. At every other listed call site, keep the file's
current child block byte-for-byte and change only the opening and closing
surface elements plus the imported component.

Apply this to:

- `ActivityPage.svelte`: `.chart-panel`.
- `activity/SummaryCards.svelte`: `.card`.
- `AnalyticsPage.svelte`: `.chart-panel`.
- `SessionHealthSection.svelte`: `.card` and `.chart-panel`.
- `analytics/SummaryCards.svelte`: `.card`.
- `InsightsPage.svelte`: `.state-panel`, `.distribution-row`, `.evidence-panel`,
  `.generated-controls`, `.inline-warning`, and `.skeleton-pattern`.
- `UsagePage.svelte`: `.usage-note` and `.chart-panel`.
- `UsagePairwiseComparisonPanel.svelte`: `.side` and `.error-bar`.
- `UsageSummaryCards.svelte`: `.card`.

For the selectable generated insight list, replace each button with:

```svelte
<Card
  level="default"
  padding="none"
  class={task.status === "error" ? "error-task" : ""}
  selected={insights.selectedTaskId === task.clientId}
  onclick={() => selectGeneratedTask(task.clientId)}
>
  <span>{task.status === "error" ? m.insights_page_error() : m.insights_page_running()}</span>
  <strong>{task.project || m.insights_page_global()}</strong>
  <em>{task.kind ? cannedKindLabel(task.kind) : task.phase}</em>
</Card>
```

Replace the saved-insight buttons with the same Card contract, using
`selected={insights.selectedId === item.id}` and
`onclick={() => selectGeneratedInsight(item.id)}` while retaining the current
type, project, date-range, and created-time children.

The `selected` prop replaces the old `active` class. Keep `error-task` as the
explicit class string shown above.

- [ ] **Step 2: Replace inset-level surfaces**

Use the same pattern with `level="inset"` for:

- `InsightsPage.svelte`: `.evidence-row`.

- `WorktreeMappingSettings.svelte`: `.mapping-row`.

- [ ] **Step 3: Remove only redundant card chrome**

From each migrated selector remove declarations for the selected Card recipe:

```css
background: var(--bg-surface); /* or --bg-inset */
border: 1px solid var(--border-muted);
border-radius: var(--radius-md); /* or --radius-sm */
box-shadow: var(--shadow-sm); /* only when the Card level owns it */
```

Retain padding, display, grid/flex, min/max sizing, overflow, border-left
accents, selected/error border overrides, responsive rules, and typography.
Update descendant selectors from element-dependent forms such as
`.generated-list button strong` to class-based selectors that still match the
Card-rendered markup.

- [ ] **Step 4: Run affected component tests and conformance**

Run:

```bash
npm test -- src/lib/components/activity/ActivityPage.test.ts src/lib/components/analytics/AnalyticsPage.test.ts src/lib/components/insights/InsightsPage.test.ts src/lib/components/settings/WorktreeMappingSettings.test.ts src/lib/components/usage/UsagePage.test.ts src/lib/components/usage/UsagePairwiseComparisonPanel.test.ts
npm run check:kit-ui
```

Expected: tests PASS and only compact control findings remain.

- [ ] **Step 5: Commit the content-surface migration**

Stage only the files listed in this task and commit:

```bash
git commit -m "refactor(frontend): adopt kit card surfaces"
```

### Task 4: Replace Compact Control False Positives with Semantic Kit Components

**Files:**

- Modify: `frontend/src/lib/components/analytics/SkillTrend.svelte`
- Modify: `frontend/src/lib/components/analytics/TopSkills.svelte`
- Modify: `frontend/src/lib/components/settings/AppearanceSettings.svelte`
- Modify: `frontend/src/lib/components/settings/EmbeddingsSettings.svelte`
- Modify: `frontend/src/lib/components/settings/GithubSettings.svelte`
- Modify: `frontend/src/lib/components/settings/RemoteSettings.svelte`
- Modify: `frontend/src/lib/components/settings/SettingsPage.svelte`
- Modify: `frontend/src/lib/components/settings/TerminalSettings.svelte`
- Modify: `frontend/src/lib/components/settings/WorktreeMappingSettings.svelte`
- Modify: `frontend/src/lib/components/insights/InsightsPage.svelte`

**Interfaces:**

- Consumes: existing kit `Button`, `Chip`, `IconButton`, `SegmentedControl`,
  `TextInput`, and `Toggle` APIs.

- Produces: zero card-rule matches without using Card for text fields, icon
  buttons, badges, or segmented choices.

- [ ] **Step 1: Migrate chips and badges**

- `SkillTrend.svelte`: retain the native toggle button because kit-ui `Chip` and
  `Button` do not expose `aria-pressed`. Add a same-line `kit-ui-check-ignore`
  comment explaining that the legend is a pressed-state toggle and the current
  kit controls cannot preserve that accessibility contract. Keep its existing
  chrome and behavior unchanged.

- `TopSkills.svelte`: use non-interactive `Chip size="xs" uppercase={false}` for
  `.agent-chip`.

- `EmbeddingsSettings.svelte`: use `Chip size="xs"` with success, warning,
  danger, or muted tone based on generation state; preserve the displayed
  status labels.

Delete the obsolete inset-card background/border/radius declarations and keep
only truncation, color-key, and layout rules that the feature still owns.

- [ ] **Step 2: Migrate text fields**

Replace the flagged native text inputs with `TextInput`, retaining ids,
password/url/text types, placeholders, bindings, Enter-key handling, monospace
classes, width, and disabled state:

- `GithubSettings.svelte`: `.setting-input`.
- `RemoteSettings.svelte`: both `.setting-input` fields.
- `SettingsPage.svelte`: `.auth-input`.
- `TerminalSettings.svelte`: both `.setting-input` fields.
- `WorktreeMappingSettings.svelte`: `.field input` fields, using `block` and
  preserving the derived disabled state.

Use `size="md"` for 28-30px controls and retain a feature class only where the
layout needs flex growth, block width, monospace font, or the 34px auth height.

- [ ] **Step 3: Migrate buttons and option groups**

- `AppearanceSettings.svelte`: use `Button size="sm"` for theme/high-contrast
  actions and `SegmentedControl` for message layout and font scale.

- `RemoteSettings.svelte`: use `Toggle` for require-auth and `Button size="sm"`
  for the text-labeled token copy action, preserving loading/disabled and
  copied-label behavior.

- `SettingsPage.svelte`: use `Button` for resync.

- `TerminalSettings.svelte`: use `SegmentedControl` for launch mode.

- `WorktreeMappingSettings.svelte`: use `SegmentedControl` for layout.

- `InsightsPage.svelte`: use `Button size="sm"` for `.header-action` and
  `IconButton size="sm" tone="danger"` for `.icon-action`; retain localized
  accessible labels for icon-only delete/dismiss actions.

Do not rewrite other buttons in these files.

- [ ] **Step 4: Run focused tests and the full conformance suite**

Run:

```bash
npm test -- src/lib/components/analytics/SkillTrend.test.ts src/lib/components/analytics/TopSkills.test.ts src/lib/components/insights/InsightsPage.test.ts src/lib/components/settings/AppearanceSettings.test.ts src/lib/components/settings/EmbeddingsSettings.test.ts src/lib/components/settings/SettingsPage.test.ts src/lib/components/settings/WorktreeMappingSettings.test.ts
npm run check:kit-ui
```

Expected: tests PASS and `kit-ui-check: 0 finding(s)` across the full rule set.

- [ ] **Step 5: Commit the compact-control migration**

Stage only this task's files and commit:

```bash
git commit -m "refactor(frontend): use semantic kit controls"
```

### Task 5: Full Verification and Visual QA

**Files:**

- Verify: all modified frontend files
- Verify: `frontend/e2e/appearance-a11y.spec.ts`
- Verify: `frontend/e2e/usage.spec.ts`
- Verify: `frontend/e2e/insights-quality.spec.ts`

**Interfaces:**

- Consumes: the completed component migration.

- Produces: fresh evidence for type safety, behavior, conformance, responsive
  layout, and task closure.

- [ ] **Step 1: Run complete frontend verification**

Run from `frontend/`:

```bash
npm run check:kit-ui
npm run check
npm test
```

Expected: all three commands PASS with zero conformance findings.

- [ ] **Step 2: Run relevant Playwright flows**

Run:

```bash
npx playwright test e2e/appearance-a11y.spec.ts e2e/usage.spec.ts e2e/insights-quality.spec.ts
```

Expected: all selected workflows PASS. If the local E2E server prerequisites are
unavailable, record the exact launch failure and run the repository's documented
E2E setup rather than weakening assertions.

- [ ] **Step 3: Perform desktop and narrow visual checks**

Use the local app at a desktop viewport around 1440×900 and a narrow viewport
around 390×844. Inspect settings, analytics/insights, trends, activity, and
usage. Confirm card padding, grid wrapping, overflow, checked/disabled states,
focus rings, labels, and selected generated-insight rows match the pre-migration
layout.

- [ ] **Step 4: Review the final diff and private-data scrub**

Run from the repository root:

```bash
git status --short
git diff --stat HEAD~4..HEAD
git diff HEAD~4..HEAD
```

Expected: only task files changed; the scrub finds no private data in published
content after manually reviewing the full diff. The design and plan documents
must refer only to repository-relative paths.

- [ ] **Step 5: Commit any verification-driven fixes**

If verification required tracked-file changes, stage only those fixes and commit
with a focused conventional message. If no files changed, do not create an empty
commit.

- [ ] **Step 6: Close kata task `pfvs` after verification**

From the repository root, after the implementation commit SHA is known:

```bash
kata close pfvs --done --message "Migrated card surfaces and boolean controls to kit-ui components; frontend conformance, type checks, tests, and relevant UI flows pass." --commit "$(git rev-parse HEAD)"
```

Do not close the task if required verification is incomplete.
