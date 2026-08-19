# Redesign project classification

## Destination

Produce an approved interaction specification, backed by throwaway UI
prototypes, for a faster and less intrusive project-classification experience
that is ready for feature-flagged implementation. The specification must tie
each approved workflow to explicit domain rules, storage behavior, interaction
states, and acceptance criteria.

## Notes

- Preserve the project inventory overview as the primary Data view.
- Make the selected-project pane the main place to add and move observed
  folders.
- Keep ordinary corrections explicit, quick, and immediately persistent. Require
  extra review only when impact is broad or surprising.
- Keep Project rules as a secondary administrative view.
- Evaluate prototypes with the agreed tasks: add a folder to an existing
  project, move a misclassified folder, create a project classification,
  recover from a mistaken correction, and understand a read-only result.
- Prefer designs where the project overview remains available, ordinary
  corrections need no manual prefix entry or trip to Project rules, broad
  impact is visible before commitment, and completion or failure is
  unambiguous.
- Preserve current parser inference and machine-scoped rule semantics unless a
  resolved UX decision proves that a backend change is necessary.
- Preserve observable behavior across SQLite and PostgreSQL/CockroachDB where
  the workflow is supported. Record explicit read-only behavior for PostgreSQL
  and DuckDB views instead of assuming SQLite mutation APIs exist there.
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

## Out of scope

- Assigning individual sessions to projects. That is session assignment, a
  related but separate workflow.
- Replacing the project inventory overview.
- Automatically applying suggested classifications.
- Redesigning parser inference unrelated to the approved project workflow.
- Shipping the production redesign as part of this wayfinding map.
