# Phase 3: Frontend Wiring - Research

**Researched:** 2026-02-28
**Domain:** Svelte 5 SPA — agent registry, CSS variables, badge class, filter UI
**Confidence:** HIGH

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions

**Teal color**
- Use standard teal family: Claude to choose exact light and dark hex values (reference: `#0d9488` light / `#2dd4bf` dark)
- Both light `:root` and dark `:root.dark` blocks in `app.css` must include `--accent-teal`
- Color must be visually distinct from all six existing accents (blue, green, amber, rose, purple, black)

**Badge label**
- Breadcrumb badge in `App.svelte` (the `{session.agent}` span) must show "Pi" capitalized, not raw "pi"
- Fix the capitalization in that span — either via CSS `text-transform: capitalize` (matching `SessionItem.svelte`) or by adding a display label mapping
- The sidebar dot label in `SessionItem.svelte` already capitalizes via `text-transform: capitalize` — no change needed there

**cursor agent gap**
- Do NOT fix the missing `agent-cursor` badge class in `App.svelte` during this phase — that is a separate concern

### Claude's Discretion
- Exact hex values for `--accent-teal` light and dark (within standard teal family)
- Whether capitalization is handled by CSS `text-transform` or a `label` field on `AgentMeta`
- Test coverage approach for the frontend changes

### Deferred Ideas (OUT OF SCOPE)
- cursor agent badge missing in App.svelte breadcrumb — noted, not in scope for Phase 3
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| FRNT-01 | `"pi"` entry added to `KNOWN_AGENTS` in `frontend/src/lib/utils/agents.ts` with display label "Pi" | Direct: add `{ name: "pi", color: "var(--accent-teal)" }` to the array; update `agentColor` map automatically |
| FRNT-02 | `--accent-teal` CSS variable added to `frontend/src/app.css` | Direct: two-block addition to `:root` (light) and `:root.dark` (dark) following the exact pattern of all five existing accent variables |
| FRNT-03 | Pi agent badge uses teal accent color in `App.svelte` | Direct: add `class:agent-pi={session.agent === "pi"}` and `.agent-pi { background: var(--accent-teal); }` matching the five existing agent badge patterns |
| FRNT-04 | Pi appears in the agent filter UI and sessions can be filtered by pi agent type | Auto-satisfied by FRNT-01: `SessionList.svelte` iterates `KNOWN_AGENTS` for filter buttons, so adding pi there is sufficient |
</phase_requirements>

---

## Summary

Phase 3 is a small, focused frontend-only change with four touch points across three files. All patterns already exist in the codebase — this phase adds a seventh entry to an established agent registry, a seventh CSS variable pair, a seventh badge class, and the filter button is generated automatically from the registry.

The existing agent system is fully understood from source inspection. `KNOWN_AGENTS` in `agents.ts` drives both the sidebar dot color (via `agentColor()`) and the filter button list in `SessionList.svelte`. The breadcrumb badge in `App.svelte` has six explicit `class:agent-X` conditionals with matching CSS classes — Pi requires a seventh. The only subtlety is the capitalization issue in the breadcrumb badge span: it renders `{session.agent}` verbatim (lowercase "pi"), unlike `SessionItem.svelte` which applies `text-transform: capitalize`. The fix should match the existing breadcrumb style — CSS `text-transform: capitalize` on `.agent-badge` is the simplest approach since it already applies `text-transform: uppercase`; switching to `capitalize` would affect all badges. A safer approach is a one-line display helper.

No external dependencies need to be added. No build process changes. The test target is `vitest run` (unit tests via jsdom) with existing test infrastructure in `frontend/src/lib/utils/agents.test.ts`.

**Primary recommendation:** Three-file change in this order — (1) `app.css` add `--accent-teal`, (2) `agents.ts` add pi entry, (3) `App.svelte` add badge class and fix capitalization. Tests update `agents.test.ts` to assert pi presence.

---

## Standard Stack

### Core

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Svelte | 5.53.5 | Component framework | Project's existing SPA framework |
| Vite | 7.3.1 | Build tool + dev server | Existing bundler |
| Vitest | 4.0.18 | Unit test runner | Already configured in `vite.config.ts` |
| TypeScript | 5.9.3 | Type safety | Project standard |

### Supporting

| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| jsdom | 28.1.0 | DOM environment for tests | Already configured in vitest `environment: "jsdom"` |

### Alternatives Considered

None — this phase uses only existing project stack. No new packages.

**Installation:** No new packages needed.

---

## Architecture Patterns

### Recommended Project Structure

No new files are required. All changes are in-place edits:

```
frontend/src/
├── app.css                          # Add --accent-teal to :root and :root.dark
├── App.svelte                       # Add agent-pi badge class and capitalization fix
└── lib/
    └── utils/
        ├── agents.ts                # Add pi entry to KNOWN_AGENTS
        └── agents.test.ts           # Update test to assert pi is present
```

### Pattern 1: Agent Registry Entry

**What:** Add a new agent to the `KNOWN_AGENTS` readonly array in `agents.ts`.
**When to use:** Any time a new agent is integrated into the backend.
**Example:**
```typescript
// Source: frontend/src/lib/utils/agents.ts (existing pattern)
export const KNOWN_AGENTS: readonly AgentMeta[] = [
  { name: "claude",    color: "var(--accent-blue)" },
  { name: "codex",     color: "var(--accent-green)" },
  { name: "copilot",   color: "var(--accent-amber)" },
  { name: "gemini",    color: "var(--accent-rose)" },
  { name: "opencode",  color: "var(--accent-purple)" },
  { name: "cursor",    color: "var(--accent-black)" },
  { name: "pi",        color: "var(--accent-teal)" },   // NEW
];
```

The `agentColorMap` is built from `KNOWN_AGENTS` at module load time — no other changes needed in `agents.ts`.

### Pattern 2: CSS Variable Pair

**What:** Add `--accent-teal` to both `:root` (light) and `:root.dark` in `app.css`.
**When to use:** Every accent color needs both a light and dark value.
**Example:**
```css
/* Source: frontend/src/app.css (existing pattern) */
:root {
  /* ... existing vars ... */
  --accent-teal: #0d9488;   /* NEW — Tailwind teal-600, visible on white */
}

:root.dark {
  /* ... existing vars ... */
  --accent-teal: #2dd4bf;   /* NEW — Tailwind teal-400, readable on dark bg */
}
```

Existing accent colors for reference (light/dark):
- blue: `#2563eb` / `#60a5fa`
- green: `#059669` / `#34d399`
- amber: `#d97706` / `#fbbf24`
- rose: `#e11d48` / `#fb7185`
- purple: `#7c3aed` / `#a78bfa`
- black: `#2d2d2d` / `#b0b0b0`

Teal `#0d9488` (light) / `#2dd4bf` (dark) is visually distinct from all six.

### Pattern 3: Breadcrumb Badge Class

**What:** Add `class:agent-pi` conditional and `.agent-pi` CSS rule in `App.svelte`.
**When to use:** Every agent that can appear in the session breadcrumb needs an explicit badge class.
**Example:**
```svelte
<!-- Source: frontend/src/App.svelte (existing pattern) -->
<span
  class="agent-badge"
  class:agent-claude={session.agent === "claude"}
  class:agent-codex={session.agent === "codex"}
  class:agent-copilot={session.agent === "copilot"}
  class:agent-gemini={session.agent === "gemini"}
  class:agent-opencode={session.agent === "opencode"}
  class:agent-pi={session.agent === "pi"}
>{session.agent}</span>
```

```css
/* In App.svelte <style> block */
.agent-pi {
  background: var(--accent-teal);
}
```

### Pattern 4: Breadcrumb Capitalization Fix

**What:** The `.agent-badge` class currently uses `text-transform: uppercase` — Pi shows as "PI". The CONTEXT.md decision says it must show "Pi".
**Analysis:** Three approaches:
1. Change `.agent-badge` to `text-transform: capitalize` — affects all badges (currently uppercase), would change "CLAUDE" → "Claude" etc. This is a cross-cutting style change.
2. Add a display helper function to format agent name for the breadcrumb badge.
3. Use a `label` field on `AgentMeta` (Claude's discretion per CONTEXT.md).

**Recommended:** Add a `label` field to `AgentMeta` with a fallback. This is the cleanest solution and does not change visual style of existing badges:
```typescript
// agents.ts
export interface AgentMeta {
  name: string;
  color: string;
  label?: string;   // optional display label, defaults to name
}
export const KNOWN_AGENTS: readonly AgentMeta[] = [
  { name: "pi", color: "var(--accent-teal)", label: "Pi" },
  // ... existing entries (no label needed — uppercase CSS handles display)
];
```

Then in `App.svelte`, replace `{session.agent}` with:
```svelte
{KNOWN_AGENTS.find(a => a.name === session.agent)?.label ?? session.agent}
```

And import `KNOWN_AGENTS` in `App.svelte`.

**Alternative (simpler):** Add a standalone `agentLabel(agent: string): string` function to `agents.ts` that returns capitalized form for known agents, falling back to the raw string. This avoids modifying the interface.

```typescript
// agents.ts
export function agentLabel(agent: string): string {
  const meta = KNOWN_AGENTS.find(a => a.name === agent);
  if (meta?.label) return meta.label;
  // Default: capitalize first letter
  return agent.charAt(0).toUpperCase() + agent.slice(1);
}
```

This is the approach that scales best if future agents need custom display names.

### Pattern 5: Filter Button Auto-Generation

**What:** `SessionList.svelte` iterates `KNOWN_AGENTS` to render filter buttons — no changes needed in `SessionList.svelte`.
**Example (existing):**
```svelte
<!-- frontend/src/lib/components/sidebar/SessionList.svelte -->
{#each KNOWN_AGENTS as agent}
  <button
    class="agent-filter-btn"
    class:active={sessions.filters.agent === agent.name}
    style:--agent-color={agent.color}
    onclick={() => sessions.setAgentFilter(agent.name)}
  >
    <span class="agent-dot-mini" style:background={agent.color}></span>
    {agent.name}
  </button>
{/each}
```

Adding `"pi"` to `KNOWN_AGENTS` automatically adds its filter button. The button label here also renders `agent.name` (lowercase "pi") — same capitalization issue. If `agentLabel()` is added to `agents.ts`, `SessionList.svelte` can be updated too (but the CONTEXT.md only requires the breadcrumb fix).

### Anti-Patterns to Avoid

- **Hardcoding teal color inline in components:** Use `var(--accent-teal)` everywhere, never raw hex. The CSS variable approach is how all six existing agents work.
- **Modifying `SessionList.svelte` to get FRNT-04:** It already reads `KNOWN_AGENTS` — the filter button appears automatically once pi is in the array.
- **Changing `.agent-badge` to `text-transform: capitalize`:** This would change all six existing badge labels from uppercase to title-case (visual regression). Use a label field or helper function instead.
- **Adding `label` to all existing `AgentMeta` entries as required fields:** Keep `label` optional to avoid breaking changes to the interface.

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Agent-to-color mapping | Manual switch/if-else in components | `agentColor()` from `agents.ts` | Already exists, used by `SessionItem.svelte` |
| Filter button list | Hardcoded JSX/Svelte per-agent | `{#each KNOWN_AGENTS}` loop | Already implemented in `SessionList.svelte` |
| Dark mode color selection | Runtime detection | CSS variable dual-block | `:root` / `:root.dark` pattern already established |

**Key insight:** The agent system is data-driven. Adding to `KNOWN_AGENTS` is the one change that propagates through sidebar dot color, filter button rendering, and `agentColor()` lookups. Component badge classes in `App.svelte` are the only place that requires a second explicit change.

---

## Common Pitfalls

### Pitfall 1: Missing Dark Mode CSS Variable

**What goes wrong:** `--accent-teal` added to `:root` but not `:root.dark` — teal shows correctly in light mode, falls back to undefined (transparent/black) in dark mode.
**Why it happens:** Easy to add to only the first block.
**How to avoid:** Always update both `:root` and `:root.dark` blocks. Check that all six existing accents have both.
**Warning signs:** Badge disappears or shows solid black in dark mode.

### Pitfall 2: Breadcrumb Badge Uppercase Override

**What goes wrong:** `text-transform: uppercase` on `.agent-badge` in `App.svelte` style block means "Pi" renders as "PI" even if the template outputs "Pi".
**Why it happens:** The breadcrumb badge has `text-transform: uppercase` — confirmed in App.svelte styles.
**How to avoid:** Use a display label that is already title-case AND override text-transform only on `.agent-pi` if needed, OR use the `agentLabel()` helper and drop `text-transform: uppercase` from `.agent-badge` (affects all). Recommend `agentLabel()` + keeping current uppercase for all other agents via adding per-badge override:
  ```css
  .agent-pi { background: var(--accent-teal); text-transform: capitalize; }
  ```
**Warning signs:** Test renders "PI" not "Pi" in the breadcrumb.

### Pitfall 3: agents.test.ts Hardcoded Array

**What goes wrong:** `agents.test.ts` has a hardcoded `.toEqual(["claude", "codex", "copilot", "gemini", "opencode", "cursor"])` — adding pi will break this test if it is not updated.
**Why it happens:** The test was written with a fixed expectation for exactly 6 agents.
**How to avoid:** Update the test to include `"pi"` in the expected array and add an assertion for `agentColor("pi")`.
**Warning signs:** `vitest run` fails with "expected claude,codex,copilot,gemini,opencode,cursor,pi to equal claude,codex,copilot,gemini,opencode,cursor".

### Pitfall 4: SessionList filter button label remains lowercase

**What goes wrong:** If the goal is for "Pi" to appear capitalized in the filter dropdown too, but only the breadcrumb is fixed.
**Why it happens:** The filter button renders `{agent.name}` (same `session.agent` pattern).
**How to avoid:** CONTEXT.md only requires breadcrumb fix. Document the inconsistency but don't fix it — it's out of scope per the locked decisions.

---

## Code Examples

### Add pi to KNOWN_AGENTS

```typescript
// frontend/src/lib/utils/agents.ts
export interface AgentMeta {
  name: string;
  color: string;
  label?: string;
}

export const KNOWN_AGENTS: readonly AgentMeta[] = [
  { name: "claude",   color: "var(--accent-blue)" },
  { name: "codex",    color: "var(--accent-green)" },
  { name: "copilot",  color: "var(--accent-amber)" },
  { name: "gemini",   color: "var(--accent-rose)" },
  { name: "opencode", color: "var(--accent-purple)" },
  { name: "cursor",   color: "var(--accent-black)" },
  { name: "pi",       color: "var(--accent-teal)", label: "Pi" },
];

export function agentLabel(agent: string): string {
  const meta = KNOWN_AGENTS.find(a => a.name === agent);
  if (meta?.label) return meta.label;
  return agent.charAt(0).toUpperCase() + agent.slice(1);
}
```

### Add --accent-teal to app.css

```css
/* frontend/src/app.css — :root block, after --accent-black */
--accent-teal: #0d9488;

/* frontend/src/app.css — :root.dark block, after --accent-black */
--accent-teal: #2dd4bf;
```

### Add agent-pi badge in App.svelte

```svelte
<!-- App.svelte template — add to class: directives -->
<span
  class="agent-badge"
  class:agent-claude={session.agent === "claude"}
  class:agent-codex={session.agent === "codex"}
  class:agent-copilot={session.agent === "copilot"}
  class:agent-gemini={session.agent === "gemini"}
  class:agent-opencode={session.agent === "opencode"}
  class:agent-pi={session.agent === "pi"}
>{agentLabel(session.agent)}</span>
```

```svelte
<!-- App.svelte script block — add import -->
import { agentLabel, KNOWN_AGENTS } from "./lib/utils/agents.js";
```

```css
/* App.svelte <style> block — add after .agent-opencode */
.agent-pi {
  background: var(--accent-teal);
  text-transform: capitalize;
}
```

### Update agents.test.ts

```typescript
// frontend/src/lib/utils/agents.test.ts
it("contains all expected agents", () => {
  const names = KNOWN_AGENTS.map((a) => a.name);
  expect(names).toEqual([
    "claude", "codex", "copilot", "gemini", "opencode", "cursor", "pi",
  ]);
});

it("returns correct color for pi", () => {
  expect(agentColor("pi")).toBe("var(--accent-teal)");
});
```

---

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Svelte 4 stores with `writable()` | Svelte 5 `$state` runes + `.svelte.ts` files | Svelte 5 | All stores use rune-based reactivity; no `writable()` imports |
| Class-based conditional CSS | Svelte `class:name={condition}` directive | Svelte 2+ | The badge pattern in App.svelte is idiomatic |

**Deprecated/outdated:**
- None relevant to this phase.

---

## Open Questions

1. **Filter button label capitalization**
   - What we know: `SessionList.svelte` renders `{agent.name}` (lowercase) in filter buttons; CONTEXT.md only requires breadcrumb fix
   - What's unclear: Whether the product expectation is "pi" or "Pi" in the filter dropdown
   - Recommendation: Follow locked decision — fix breadcrumb only, leave filter as-is. Document as known inconsistency.

2. **`text-transform: capitalize` scope**
   - What we know: `.agent-badge` uses `text-transform: uppercase` so ALL badges display uppercase; adding `text-transform: capitalize` on `.agent-pi` only overrides for pi
   - What's unclear: Whether adding per-class override is cleaner than using `agentLabel()` to supply the correctly-cased string and removing the uppercase rule
   - Recommendation: Use `agentLabel()` + per-badge override `.agent-pi { text-transform: capitalize; }`. This keeps existing badges unaffected and is explicit.

---

## Sources

### Primary (HIGH confidence)

- Direct source inspection: `frontend/src/lib/utils/agents.ts` — KNOWN_AGENTS array, AgentMeta interface, agentColor function
- Direct source inspection: `frontend/src/app.css` — complete CSS variable inventory for light and dark themes
- Direct source inspection: `frontend/src/App.svelte` — badge class pattern with all 5 existing agent conditionals
- Direct source inspection: `frontend/src/lib/components/sidebar/SessionList.svelte` — filter button generation from KNOWN_AGENTS
- Direct source inspection: `frontend/src/lib/components/sidebar/SessionItem.svelte` — sidebar dot uses agentColor(), text-transform: capitalize
- Direct source inspection: `frontend/src/lib/utils/agents.test.ts` — hardcoded agent name array that needs updating

### Secondary (MEDIUM confidence)

- Tailwind CSS teal scale — `#0d9488` is teal-600 (light), `#2dd4bf` is teal-400 (dark) — standard teal values matching the reference hex in CONTEXT.md

---

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — all files read directly from source
- Architecture: HIGH — patterns are explicit in existing code; no speculation
- Pitfalls: HIGH — identified from direct reading of existing test assertions and CSS rules

**Research date:** 2026-02-28
**Valid until:** 2026-03-28 (stable codebase, no moving dependencies)
