# Worktree Tooltip Width Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> to implement this plan directly in the current agent, task-by-task. Never use
> subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the session-vitals worktree tooltip use up to half of the viewport
so ordinary paths stay on one line while oversized paths wrap.

**Approved spec/design:**
`docs/superpowers/specs/2026-07-30-worktree-tooltip-width-design.md`

**Architecture:** Keep kit-ui's generic tooltip sizing unchanged. Pass a
dedicated class to the existing worktree `Tooltip`, then use the supported class
hook to override only that popover's maximum width while retaining kit-ui's
fixed viewport positioning and end-aligning its arrow with the trigger.

**Tech Stack:** Svelte 5, TypeScript, `@kenn-io/kit-ui`, Playwright, Vitest

## Global Constraints

- The generic kit-ui tooltip must retain its 280-pixel default maximum width.
- The worktree tooltip maximum width is `min(50vw, calc(100vw - 32px))`.
- Paths that fit within the cap stay on one line; longer paths wrap.
- The tooltip may extend left of the analysis sidebar but must remain inside the
  viewport.
- Unbroken path segments must wrap rather than overflow, and the tooltip arrow
  must remain over the worktree trigger.
- Preserve the existing LTR-isolated path text, start truncation, hover and
  keyboard behavior, copy control, and empty state.

______________________________________________________________________

### Task 1: Make the worktree tooltip viewport-relative

**Files:**

- Modify: `cmd/testfixture/main.go`
- Modify: `frontend/e2e/session-timing.spec.ts`
- Modify: `frontend/src/lib/components/content/SessionVitals.svelte`

**Interfaces:**

- Consumes: kit-ui `Tooltip`'s supported `class?: string` and `align` props plus
  existing fixed-position viewport clamping.

- Produces: a `worktree-path-tooltip` popover class scoped to the session-vitals
  worktree tooltip.

- [ ] **Step 1: Make the fixture deterministic and write the failing browser
  test**

In `cmd/testfixture/main.go`, lengthen the showcase session's existing synthetic
worktree path to exactly this value:

```go
Cwd: "/workspace/مشروع/.worktrees/שלוםfeaturewithalongcheckoutnamefortooltipwrappingwithoutbreakopportunities",
```

Update `SHOWCASE_WORKTREE` in `frontend/e2e/session-timing.spec.ts` to the same
literal:

```ts
const SHOWCASE_WORKTREE =
  "/workspace/مشروع/.worktrees/שלוםfeaturewithalongcheckoutnamefortooltipwrappingwithoutbreakopportunities";
```

Add this test after the existing mixed-direction worktree-path regression in
`frontend/e2e/session-timing.spec.ts`:

```ts
test("uses available viewport width for the worktree tooltip", async ({
  page,
}) => {
  await gotoShowcase(page);

  const panel = page.locator("aside.vitals");
  const path = page.locator(".context-value--path");
  const trigger = page.locator(".context-tooltip .kit-tooltip-trigger");
  await path.hover();

  const tooltip = page.getByRole("tooltip");
  await expect(tooltip).toHaveText(SHOWCASE_WORKTREE);
  const panelLeft = await panel.evaluate(
    (element) => element.getBoundingClientRect().left,
  );
  const wideLayout = await tooltip.evaluate((element) => {
    const range = document.createRange();
    range.selectNodeContents(element);
    const lineTops = Array.from(
      range.getClientRects(),
      (rect) => Math.round(rect.top),
    );
    const rect = element.getBoundingClientRect();
    const arrowStyle = getComputedStyle(element, "::before");
    const arrowWidth = Number.parseFloat(arrowStyle.width);
    const arrowCenter =
      arrowStyle.left === "auto"
        ? rect.right - Number.parseFloat(arrowStyle.right) - arrowWidth / 2
        : rect.left + Number.parseFloat(arrowStyle.left) + arrowWidth / 2;
    return {
      arrowCenter,
      left: rect.left,
      lineCount: new Set(lineTops).size,
      right: rect.right,
      viewportWidth: window.innerWidth,
      width: rect.width,
    };
  });
  const triggerBounds = await trigger.boundingBox();
  expect(triggerBounds).not.toBeNull();

  expect(wideLayout.width).toBeLessThanOrEqual(
    wideLayout.viewportWidth * 0.5 + 1,
  );
  expect(wideLayout.lineCount).toBe(1);
  expect(wideLayout.left).toBeGreaterThanOrEqual(0);
  expect(wideLayout.right).toBeLessThanOrEqual(wideLayout.viewportWidth);
  expect(wideLayout.left).toBeLessThan(panelLeft);
  expect(wideLayout.arrowCenter).toBeGreaterThanOrEqual(triggerBounds!.x);
  expect(wideLayout.arrowCenter).toBeLessThanOrEqual(
    triggerBounds!.x + triggerBounds!.width,
  );

  await page.setViewportSize({ width: 1000, height: 900 });
  await expect(path).toBeVisible();
  await path.hover();
  const narrowLayout = await tooltip.evaluate((element) => {
    const range = document.createRange();
    range.selectNodeContents(element);
    const lineTops = Array.from(
      range.getClientRects(),
      (rect) => Math.round(rect.top),
    );
    const rect = element.getBoundingClientRect();
    return {
      clientWidth: element.clientWidth,
      left: rect.left,
      lineCount: new Set(lineTops).size,
      right: rect.right,
      scrollWidth: element.scrollWidth,
      viewportWidth: window.innerWidth,
      width: rect.width,
    };
  });

  expect(narrowLayout.width).toBeLessThanOrEqual(
    narrowLayout.viewportWidth * 0.5 + 1,
  );
  expect(narrowLayout.lineCount).toBeGreaterThan(1);
  expect(narrowLayout.scrollWidth).toBeLessThanOrEqual(
    narrowLayout.clientWidth + 1,
  );
  expect(narrowLayout.left).toBeGreaterThanOrEqual(0);
  expect(narrowLayout.right).toBeLessThanOrEqual(narrowLayout.viewportWidth);
});
```

- [ ] **Step 2: Run the browser test to verify it fails**

Run:

```bash
cd frontend
npm run e2e -- session-timing.spec.ts --project=chromium \
  --grep "uses available viewport width for the worktree tooltip"
```

Expected: FAIL because the current 280-pixel maximum wraps the fixture path at
the default 1600-pixel viewport, so `wideLayout.lineCount` is greater than 1.
After adding only the width override, the arrow assertion also fails until the
tooltip is end-aligned.

- [ ] **Step 3: Add the per-use width override**

In `frontend/src/lib/components/content/SessionVitals.svelte`, pass the popover
class through the existing tooltip:

```svelte
<Tooltip
  text={session.cwd}
  focusable
  align="end"
  class="worktree-path-tooltip"
>
```

Add the scoped global rule next to the existing `.context-tooltip` integration
styles:

```css
.context-tooltip :global(.worktree-path-tooltip) {
  max-width: min(50vw, calc(100vw - 32px));
  overflow-wrap: anywhere;
}
```

Do not modify kit-ui source or the shared `.kit-tooltip` rule.

- [ ] **Step 4: Run the focused browser regression in both engines**

Run:

```bash
cd frontend
npm run e2e -- session-timing.spec.ts --project=chromium \
  --grep "uses available viewport width for the worktree tooltip"
npm run e2e -- session-timing.spec.ts --project=webkit \
  --grep "uses available viewport width for the worktree tooltip"
```

Expected: PASS in Chromium and WebKit. The default viewport renders one line and
positions the tooltip left of the sidebar; the 1000-pixel viewport wraps the
same complete path without exceeding 500 pixels or overflowing its unbroken
segment. Both layouts remain viewport-contained, with the arrow over the
trigger.

- [ ] **Step 5: Run the affected component and integration checks**

Run:

```bash
cd frontend
npm test -- --run src/lib/components/content/SessionVitals.test.ts
npm run e2e -- session-timing.spec.ts --project=chromium
npm run check
npm run check:kit-ui
cd ..
go fmt ./...
go vet ./...
```

Expected: 12 component tests and all 7 Chromium session-vitals tests pass,
Svelte reports 0 errors, kit-ui reports 0 findings, and Go formatting and vet
complete successfully. Existing unrelated Svelte unused-selector warnings may
remain.

- [ ] **Step 6: Commit the implementation**

```bash
git add cmd/testfixture/main.go \
  frontend/e2e/session-timing.spec.ts \
  frontend/src/lib/components/content/SessionVitals.svelte
git commit -m "fix: widen worktree path tooltip" \
  -m "The shared 280-pixel default suits prose but forces filesystem paths to wrap inside the analysis sidebar. Give only the worktree tooltip a viewport-relative limit so ordinary paths stay on one line while oversized paths wrap at half the viewport."
```
