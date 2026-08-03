# Sidebar Toggle Placement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> to implement this plan directly in the current agent, task-by-task. Never use
> subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the desktop sidebar toggle next to the session filter while
preserving the existing mobile hamburger behavior.

**Approved spec/design:** This plan is based directly on the user's approved
requirements: with the desktop sidebar open, render the collapse control to the
right of the filter; with it closed, render the expand control to the left of
the relocated filter; keep the current mobile hamburger and drawer behavior.

**Architecture:** Add one shared state-aware sidebar toggle built from kit-ui's
`IconButton`, then render it at the two session-list boundaries that own the
filter. The global header keeps the hamburger only on mobile. Session detail
receives the same closed-state toggle/filter pair in its breadcrumb. Focus moves
to the newly visible counterpart after a keyboard-initiated toggle, and each
control exposes the sidebar relationship through `aria-expanded` and
`aria-controls`, so closing the sidebar never strands the user.

**Tech Stack:** Svelte 5, TypeScript, kit-ui, Lucide icons, Paraglide JS, Vitest
through Vite+

## Global Constraints

- Do not change the existing mobile hamburger route and drawer behavior, and do
  not render the relocated controls on mobile.
- Use shared controls rather than adding one-off button chrome.
- Keep every locale catalog's message keys identical.
- Tests must assert rendered order and click behavior, not source text.

______________________________________________________________________

### Task 1: Protect the responsive placement contract

**Files:**

- Modify: `frontend/src/lib/components/layout/AppHeader.test.ts`
- Modify: `frontend/src/lib/components/sidebar/SessionList.test.ts`
- Modify: `frontend/src/lib/components/analytics/AnalyticsPage.test.ts`
- Modify: `frontend/src/lib/components/layout/SessionBreadcrumb.test.ts`
- Create: `frontend/src/lib/components/layout/SidebarToggleButton.test.ts`

**Interfaces:**

- Consumes: current `ui.sidebarOpen`, `ui.isMobileViewport`, and rendered filter
  controls.

- Produces: regression coverage for desktop header removal, both mobile
  hamburger branches, open-state filter/collapse order, closed-state
  expand/filter order, and keyboard focus handoff in both directions.

- [ ] **Step 1: Install the pinned frontend dependencies**

Run:

```bash
cd frontend && vp install -- --allow-git=root
```

Expected: dependencies install successfully without changing the pinned
dependency set.

- [ ] **Step 2: Write failing rendered-behavior tests**

Add tests that query the localized `aria-label` values, compare sibling order
around `.filter-btn`, click the toggle, and assert the observable
`ui.sidebarOpen` state change. Focused-button tests must assert that focus moves
to the newly visible counterpart after both collapse and reopen. Mobile
AppHeader tests must cover closing the drawer on the sessions route and
navigating from another route to open it. Mobile SessionList, Analytics, and
Breadcrumb tests must prove the relocated controls are absent. Each desktop
relocated control must retain the existing localized `Toggle sidebar (b)` title
so the keyboard shortcut remains discoverable.

- [ ] **Step 3: Run the focused tests and verify RED**

Run:

```bash
cd frontend && PATH="$(pwd)/node_modules/.bin:$PATH" vp test src/lib/components/layout/AppHeader.test.ts src/lib/components/layout/SidebarToggleButton.test.ts src/lib/components/sidebar/SessionList.test.ts src/lib/components/analytics/AnalyticsPage.test.ts src/lib/components/layout/SessionBreadcrumb.test.ts
```

Expected: assertion failures because the desktop hamburger is still global and
the filter-adjacent controls do not exist.

### Task 2: Add the shared sidebar toggle and place it around the filter

**Files:**

- Create: `frontend/src/lib/components/layout/SidebarToggleButton.svelte`
- Modify: `frontend/src/lib/icons.ts`
- Modify: `frontend/src/lib/icons.test.ts`
- Modify: `frontend/src/lib/components/layout/AppHeader.svelte`
- Modify: `frontend/src/lib/components/layout/ThreeColumnLayout.svelte`
- Modify: `frontend/src/lib/components/sidebar/SessionList.svelte`
- Modify: `frontend/src/lib/components/analytics/AnalyticsPage.svelte`
- Modify: `frontend/src/lib/components/layout/SessionBreadcrumb.svelte`
- Modify: `frontend/messages/en.json`
- Modify: `frontend/messages/zh-CN.json`
- Modify: `frontend/messages/zh-TW.json`
- Modify: `frontend/messages/ko.json`
- Modify: `frontend/messages/fr.json`

**Interfaces:**

- Consumes: `ui.sidebarOpen`, `ui.toggleSidebar()`, `m.nav_open_sidebar()`, and
  `m.nav_close_sidebar()`.

- Produces: `SidebarToggleButton`, a shared icon-only button that renders the
  correct panel-open/panel-close icon and accessible state-specific label.

- [ ] **Step 1: Add localized open-sidebar copy**

Add `nav_open_sidebar` to every locale next to `nav_close_sidebar`, with an
accurate translation in each catalog.

- [ ] **Step 2: Implement the shared control**

Create `SidebarToggleButton.svelte` with kit-ui `IconButton`. It calls
`ui.toggleSidebar()`, labels itself from the current state, retains
`m.nav_toggle_sidebar_shortcut()` as its title, and uses `PanelLeftCloseIcon`
while open and `PanelLeftOpenIcon` while closed. Export both icons through the
app icon facade and add them to its allowlist. Identify whether each toggle is
in the sidebar or content region, expose `aria-expanded` and `aria-controls`,
and move keyboard focus to the opposite region's toggle after the state change.
Give the layout sidebar the stable ID referenced by the controls.

- [ ] **Step 3: Relocate the desktop control**

Render the AppHeader hamburger only when `ui.isMobileViewport`. On desktop only,
place `SidebarToggleButton` immediately after `SessionFilterControl` in
`SessionList`. On desktop with the sidebar collapsed, place
`SidebarToggleButton` immediately before `SessionFilterControl` in the Analytics
toolbar and SessionBreadcrumb. Give the breadcrumb pair a positioned flex
wrapper, and configure its filter with `showDisplay={false}`,
`showStarred={false}`, and `align="left"` so its dropdown has a valid anchor and
exposes only working filters.

- [ ] **Step 4: Run the focused tests and verify GREEN**

Run the same focused test command from Task 1. Expected: all selected tests
pass.

### Task 3: Verify the frontend, commit, push, and open the pull request

**Files:**

- Verify all files listed above.

**Interfaces:**

- Consumes: the completed responsive placement implementation.

- Produces: compiled locale output, repository-required frontend validation, one
  focused commit, a pushed feature branch, and an open pull request.

- [ ] **Step 1: Compile localization and run frontend checks**

```bash
cd frontend && npm run i18n:compile && PATH="$(pwd)/node_modules/.bin:$PATH" vp check && PATH="$(pwd)/node_modules/.bin:$PATH" vp test && PATH="$(pwd)/node_modules/.bin:$PATH" vp run check:kit-ui
```

Expected: locale compilation, formatting, linting, type checking, the full
frontend suite (including locale key parity), and the kit-ui contract check all
exit successfully.

- [ ] **Step 2: Re-run the focused regression tests**

```bash
cd frontend && PATH="$(pwd)/node_modules/.bin:$PATH" vp test src/lib/components/layout/AppHeader.test.ts src/lib/components/sidebar/SessionList.test.ts src/lib/components/analytics/AnalyticsPage.test.ts src/lib/components/layout/SessionBreadcrumb.test.ts
```

Expected: all selected tests pass with no failures.

- [ ] **Step 3: Check the responsive layout visually**

At the minimum desktop width, verify the open and collapsed control groups in
English and a locale with longer labels. Confirm that the sidebar header and
analytics toolbar do not overlap, clip, or wrap unexpectedly.

- [ ] **Step 4: Review the diff and commit**

Stage only the plan, component, catalog, and test changes, then create a focused
conventional commit explaining why the toggle belongs with the panel controls
and why mobile remains unchanged.

- [ ] **Step 5: Push and open the pull request**

The user already authorized the commit-push-PR workflow. Push the current
feature branch to `origin`, run the private-data scrub on the proposed
title/body, then create a pull request whose description summarizes the behavior
and rationale without a test-plan section or checklist.
