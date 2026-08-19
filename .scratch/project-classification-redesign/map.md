# Redesign project classification

## Destination

Produce an approved interaction specification, backed by throwaway UI
prototypes, for a faster and less intrusive project-classification experience
that is ready for feature-flagged implementation.

## Notes

- Preserve the project inventory overview as the primary Data view.
- Make the selected-project pane the main place to add and move observed
  folders.
- Keep ordinary corrections explicit, quick, and immediately persistent. Require
  extra review only when impact is broad or surprising.
- Keep Project rules as a secondary administrative view.
- Preserve current parser inference and machine-scoped rule semantics unless a
  resolved UX decision proves that a backend change is necessary.
- Suggestions may reduce path entry, but Agents View must never apply a project
  rule without an explicit user action.
- Consult `mattpocock-skills:grilling` and `mattpocock-skills:domain-modeling`
  for decision tickets.
- Consult `mattpocock-skills:prototype` and `impeccable` for prototype tickets.
- Consult `localization-paraglide` when prototype copy graduates into production
  UI.
- This map plans the production redesign. Prototype assets are allowed;
  production implementation is a later handoff.

## Decisions so far

## Not yet specified

- The prototypes may reveal whether the overview needs new classification
  signals, batch entry points, or no visible changes at all.
- Folder discovery may need ranking, filtering, or grouping beyond the current
  path-prefix suggestions. The chosen interaction will determine which
  questions are real.
- Performance limits for archive-wide folder discovery and impact previews
  depend on the selected workflow and backend contract.

## Out of scope

- Assigning individual sessions to projects. That is session assignment, a
  related but separate workflow.
- Replacing the project inventory overview.
- Automatically applying suggested classifications.
- Redesigning parser inference unrelated to the approved project workflow.
- Shipping the production redesign as part of this wayfinding map.
