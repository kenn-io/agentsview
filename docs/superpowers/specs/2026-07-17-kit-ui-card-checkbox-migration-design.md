# Kit UI Card and Checkbox Migration Design

## Context

The current frontend pins `@kenn-io/kit-ui` before its `Card`, `Checkbox`, and
`Toggle` components and the associated conformance rules. Running the current
kit-ui checker against the frontend reports 41 findings: 37 matching card
surfaces and four native checkbox controls.

The migration must adopt the shared components without restyling the affected
screens or changing their interactions. Existing feature-specific layout and
state styling remains app-owned; shared surface and boolean-control chrome moves
to kit-ui.

## Dependency

Update the kit-ui pin and lockfile to commit `a362f4e`, the current main commit
when the task was claimed. This includes the component introduction and the
subsequent accessibility contract fixes available at task time.

## Card Migration

Replace each conformance-matched surface with `Card` imported directly from
`@kenn-io/kit-ui`.

- Use `level="default"` for the existing `bg-surface`, `border-muted`, and
  `radius-md` recipe.
- Use `level="inset"` for the existing `bg-inset`, `border-muted`, and
  `radius-sm` recipe.
- Use `padding="none"` when the existing component owns bespoke padding,
  grid/flex layout, overflow, or responsive spacing. Use kit padding only when
  it exactly matches the current token-based spacing.
- Pass the existing feature class to `Card` so sizing, layout, responsive
  behavior, and feature-specific states remain unchanged.
- Remove only the local background, border, radius, and redundant shadow
  declarations now provided by the selected `Card` level. Retain layout,
  typography, dimensions, colors, overflow, and state selectors that remain
  feature-specific.
- Do not use structured `Card` header or footer props unless the existing markup
  already matches those regions without changing DOM order or layout.

No local wrapper component is introduced. The app does not add behavior to these
surfaces, so a wrapper would duplicate kit-ui's abstraction and make the
component source less obvious.

## Checkbox and Toggle Migration

Replace the four native checkbox controls according to their user-facing
semantics:

- Appearance block visibility uses `Checkbox`: each control selects whether a
  transcript block category is included.
- Worktree mapping enabled uses `Checkbox`: the value is part of editable form
  state saved with the mapping.
- Linked date ranges uses `Toggle`: the setting takes effect immediately as an
  on/off preference.
- Trend normalization uses `Toggle`: the setting immediately changes the
  displayed/query state.

Keep localized labels at the call sites. Preserve `bind:checked` or controlled
`checked`/`onchange` behavior as appropriate, along with existing disabled and
accessible-label contracts. Remove local label/input styles that duplicate the
kit controls, retaining only surrounding layout rules that are still needed.

## Testing and Verification

The dependency bump enables the new conformance rules and establishes the TDD
red state: `npm run check:kit-ui` must report the known 41 findings before the
migration. After implementation it must report zero findings across the full
rule set, including zero `hand-rolled-card` and zero `hand-rolled-checkbox`
findings.

Run `npm run check` and `npm test` from `frontend/`. Run focused existing tests
and relevant Playwright flows for any touched workflow with applicable coverage.
Do not add tests that merely retest kit-ui component behavior. Add a focused app
test only if the migration changes an app-owned interaction seam that existing
tests do not protect.

Visually inspect affected representative screens at desktop and narrow viewport
widths. Cover at least settings, analytics/insights, trends, and usage/activity
surfaces, checking spacing, overflow, control labels, checked states, and
disabled behavior.

## Scope Boundaries

- Do not redesign card hierarchy or restyle affected screens.
- Do not refactor unrelated frontend code.
- Do not globally disable either conformance rule.
- Add a `kit-ui-check-ignore` marker only if implementation uncovers a genuine
  component gap, and document the specific missing capability beside it.
- Do not change user-facing copy or locale catalogs as part of this migration.
