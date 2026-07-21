# Trace Repository Context Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show and copy the repository label and trace-recorded worktree path in
the session analysis sidebar.

**Architecture:** Reuse the active hydrated session already owned by the
sessions store. Pass it into `SessionVitals`, render its identity metadata above
the timing statistics, and leave the timing API unchanged.

**Tech Stack:** Svelte 5, TypeScript, Vitest, Testing Library, Paraglide JS.

## Global Constraints

- Keep the analysis pane compact and consistent with its existing label/value
  vocabulary.
- Use the exact persisted `project` and trace-recorded `cwd` values.
- Reveal shared copy controls on hover and keyboard focus.
- Localize all new user-facing labels in every supported locale.
- Do not add a new backend request or timing-response field.

______________________________________________________________________

### Task 1: Render repository context in SessionVitals

**Files:**

- Modify: `frontend/src/lib/api/types/core.ts`
- Modify: `frontend/src/App.svelte`
- Modify: `frontend/src/lib/components/content/SessionVitals.svelte`
- Test: `frontend/src/lib/components/content/SessionVitals.test.ts`
- Modify: `frontend/messages/en.json`
- Modify: `frontend/messages/zh-CN.json`
- Modify: `frontend/messages/zh-TW.json`
- Modify: `frontend/messages/ko.json`
- Modify: `frontend/messages/fr.json`

**Interfaces:**

- Consumes: `sessions.activeSession: Session | undefined`, with
  `project: string` and `cwd?: string`.

- Produces: `SessionVitals` props
  `{ sessionId: string; session: Session | undefined }` and localized
  repository-context rows.

- [ ] **Step 1: Write the failing component test**

Add tests that mount `SessionVitals` with a hydrated session whose project is
`agentsview` and whose cwd is `/repos/agentsview/.worktrees/trace-context`.
Assert that both localized labels and both values render, that the values retain
the complete strings in their title attributes, and that the hover controls copy
the corresponding values.

- [ ] **Step 2: Run the focused test to verify it fails**

Run: `npm test -- src/lib/components/content/SessionVitals.test.ts`

Expected: FAIL because the repository-context labels and values are not rendered
yet.

- [ ] **Step 3: Implement the minimal data and UI path**

Add `cwd?: string` to the frontend `Session` type, pass the active session from
`App.svelte`, accept it in `SessionVitals`, and render the two rows only when a
hydrated session is available. Add compact stacked-row styling with ellipsis and
full-value title attributes, plus shared `CopyButton` controls that reveal on
row hover or keyboard focus.

- [ ] **Step 4: Localize the labels**

Add the repository/worktree labels and copy-state messages to all five locale
catalogs, keeping the key sets identical.

- [ ] **Step 5: Run focused and frontend verification**

Run:

```bash
npm test -- src/lib/components/content/SessionVitals.test.ts
npm run i18n:compile
npm run check
npm test
```

Expected: all commands exit successfully with no new warnings attributable to
this change.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/api/types/core.ts frontend/src/App.svelte \
  frontend/src/lib/components/content/SessionVitals.svelte \
  frontend/src/lib/components/content/SessionVitals.test.ts \
  frontend/messages/en.json frontend/messages/zh-CN.json \
  frontend/messages/zh-TW.json frontend/messages/ko.json \
  frontend/messages/fr.json
git commit -m "feat(frontend): show trace repository context"
```
