# Phase 3: Frontend Wiring - Context

**Gathered:** 2026-02-28
**Status:** Ready for planning

<domain>
## Phase Boundary

Pi sessions surface in the UI with a distinct teal badge and a working agent filter. This covers adding Pi to `KNOWN_AGENTS`, defining a `--accent-teal` CSS variable, adding the `agent-pi` badge class in `App.svelte`, and updating the breadcrumb badge text to capitalize the agent name. Creating sessions and all other features are out of scope.

</domain>

<decisions>
## Implementation Decisions

### Teal color
- Use standard teal family: Claude to choose exact light and dark hex values (reference: `#0d9488` light / `#2dd4bf` dark)
- Both light `:root` and dark `:root.dark` blocks in `app.css` must include `--accent-teal`
- Color must be visually distinct from all six existing accents (blue, green, amber, rose, purple, black)

### Badge label
- Breadcrumb badge in `App.svelte` (the `{session.agent}` span) must show "Pi" capitalized, not raw "pi"
- Fix the capitalization in that span — either via CSS `text-transform: capitalize` (matching `SessionItem.svelte`) or by adding a display label mapping
- The sidebar dot label in `SessionItem.svelte` already capitalizes via `text-transform: capitalize` — no change needed there

### cursor agent gap
- Do NOT fix the missing `agent-cursor` badge class in `App.svelte` during this phase — that is a separate concern

### Claude's Discretion
- Exact hex values for `--accent-teal` light and dark (within standard teal family)
- Whether capitalization is handled by CSS `text-transform` or a `label` field on `AgentMeta`
- Test coverage approach for the frontend changes

</decisions>

<specifics>
## Specific Ideas

- No specific design references — standard teal, similar quality to existing accent colors

</specifics>

<deferred>
## Deferred Ideas

- cursor agent badge missing in App.svelte breadcrumb — noted, not in scope for Phase 3

</deferred>

---

*Phase: 03-frontend-wiring*
*Context gathered: 2026-02-28*
