# Redesign project classification

## Destination

Produce an approved interaction specification, backed by throwaway UI
prototypes, for a faster and less intrusive project-classification experience
that is ready for feature-flagged implementation. The specification must tie
each approved workflow to explicit domain rules, storage behavior, interaction
states, and acceptance criteria.

## Notes

- Preserve the project inventory overview as the primary Data view.
- Make the selected-project pane the main place to correct project
  classification from folder suggestions.
- Keep ordinary corrections explicit, quick, and immediately persistent. Require
  extra review only when impact is broad or surprising.
- Keep Project mapping rules as a secondary administrative view.
- Evaluate prototypes with the agreed tasks: use a folder suggestion for an
  existing project, correct a misclassified path, create a project
  classification, recover from a mistaken correction, and understand a
  read-only result.
- Prefer designs where the project overview remains available, ordinary
  corrections need no manual prefix entry or trip to Project mapping rules,
  broad impact is visible before commitment, and completion or failure is
  unambiguous.
- Preserve current parser inference and machine-scoped rule semantics unless a
  resolved UX decision proves that a backend change is necessary.
- Preserve observable behavior across SQLite and PostgreSQL/CockroachDB where
  the workflow is supported. Record explicit read-only behavior for PostgreSQL
  and DuckDB views instead of assuming SQLite mutation APIs exist there.
- Suggestions may reduce path entry, but Agents View must never apply a project
  mapping rule without an explicit user action.
- Consult `mattpocock-skills:grilling` and `mattpocock-skills:domain-modeling`
  for decision tickets.
- Consult `mattpocock-skills:prototype` and `impeccable` for prototype tickets.
- Consult `localization-paraglide` when prototype copy graduates into production
  UI.
- This map plans the production redesign. Prototype assets are allowed;
  production implementation is a later handoff.

## Decisions so far

- [Audit current project-classification capabilities](issues/12-audit-current-project-classification-capabilities.md):
  Reads span SQLite and the mirrors; conflict-checked preview and
  transactional apply are SQLite-only, and there is no reversal API.
- [Define project-classification jobs and invariants](issues/01-define-project-classification-jobs-and-invariants.md):
  Machine-scoped folder rules drive mapping, ordinary saves stay direct, broad
  changes get review, and stored classification layers support linear Undo.
- [Prototype project workspace variants](issues/02-prototype-project-workspace-variants.md):
  Use Split inspector so the full inventory remains visible beside the
  selected project's folders and correction controls.
- [Prototype folder triage as a complementary workflow](issues/03-prototype-folder-triage.md):
  Reject folder triage because the product has no evidence-based problem state
  from which to build a trustworthy queue.
- [Choose project classification language](issues/04-choose-project-classification-language.md):
  Frame the common task as a project correction, call evidence-backed prefixes
  folder suggestions, and reserve project mapping rule for the durable policy.
- [Define save and impact-review behavior](issues/05-define-save-and-impact-review.md):
  Save ordinary corrections directly, gate surprising scope with inline
  review, show all-time and 30-day impact, and distinguish pre-commit failure
  from a post-commit refresh failure.

## Not yet specified

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
