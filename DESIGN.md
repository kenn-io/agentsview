# Design System

agentsview is a dense local-first product UI. The interface should use a small,
consistent component vocabulary rather than one-off styling for each new
control.

## Component Rules

- Before adding any interactive frontend control, search
  `frontend/src/lib/components` for an existing component that already covers
  the behavior.
- Prefer existing shared components over native controls with local CSS.
- Do not add new hand-styled native `<select>` controls. Use the shared
  typeahead/combobox components for single-choice selectors unless there is a
  documented reason the native control is required.
- Do not introduce component-specific control chrome such as `.foo-select`,
  manual select chevrons, or one-off input/button height and padding rules when
  a shared control or token should own the styling.
- If a new control pattern is genuinely needed, add or extend a shared
  component first, then use it from feature components.

## Current Shared Controls

- `frontend/src/lib/components/layout/OptionTypeahead.svelte` is the default
  compact single-select/typeahead control for bounded option lists.
- `frontend/src/lib/components/layout/ProjectTypeahead.svelte` wraps
  `OptionTypeahead` for project selection.
- `frontend/src/lib/components/shared/RangePicker.svelte` owns date range
  selection.
- `frontend/src/lib/components/shared/RefreshControl.svelte` owns refresh
  actions and freshness display.
- `frontend/src/lib/components/settings/SettingsSection.svelte` owns settings
  section framing.

## Legacy Exceptions

Some existing activity components still contain native `<select>` controls and
manual control CSS. Treat those as legacy debt, not precedent for new work.
