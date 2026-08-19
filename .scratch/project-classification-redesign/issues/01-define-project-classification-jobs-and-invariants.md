# Define project-classification jobs and invariants

Type: grilling

Status: resolved

Blocked by: 12

## Question

Which user jobs and domain rules must every design satisfy before interaction
details are chosen? Define the identity and current state of an observed folder,
mixed-classification folders, nested or overlapping prefixes, missing or renamed
folders, rule precedence, parser-inferred classifications, and the relationship
to any individual session correction. Set representative tasks and observable
evaluation criteria, but do not select the exact interaction flow.

## Answer

### User jobs and evaluation

Every prototype must let a user add an observed folder to an existing project,
move a misclassified folder, create a project classification, recover from the
latest mistaken correction, and understand a read-only result.

The project inventory remains visible and primary. An ordinary correction uses
the generated path suggestion, requires one explicit save, and does not require
manual prefix entry or a trip to Project rules. Broad impact must be visible
before commitment, and success, failure, or a read-only result must be
unambiguous. These constraints do not select the final interaction flow.

### Folder and rule model

- An observed folder is identified by its machine and normalized path. Session
  evidence defines whether it is observed. A missing folder remains relevant
  to historical sessions, and a renamed path is a separate observed folder.
- A folder is mapped only when a project rule produces a classification for it.
  Session classifications do not map a folder. An unmapped parent may contain
  mapped children.
- Project rules are machine-specific path-prefix rules. The longest matching
  rule that successfully produces a project wins. A dynamic rule that cannot
  resolve its expected layout falls through to broader matching rules.
- A folder correction evaluates current sessions at or below its prefix on the
  same machine. More specific successful rules and session assignments retain
  precedence.
- Precedence is session assignment, project rule, then inferred classification.
  A folder correction never creates, changes, or removes a session assignment.
  Session-assignment interaction remains outside this redesign.
- Existing generated path suggestions remain the ordinary defaults. Suggestions
  for disposable leaves such as `issue-765` or `pr-887` should lift to the
  stable parent folder. Suggestions never apply a rule without explicit user
  action.

### Persistence and recovery

- The database must retain inferred classification, versioned project rule sets,
  session assignments when that separate feature exists, and an effective
  classification that can be reconstructed from those layers.
- The project rule set has one archive-wide linear history. Each saved change
  creates a version. Undo walks backward one version at a time, including
  across changes made to different machines.
- Creating, changing, disabling, deleting, or undoing a rule recomputes the
  effective classification of all current affected sessions. This includes
  sessions created after the original change. The rule-set change and
  recomputation must commit atomically.
- If the history is A then B, Undo returns to A. Saving C after that produces
  the active line A to C. B remains an audit record, not a Redo or selective
  restore target.
- The first draft includes immediate Undo for the latest change. It does not
  include a history browser, Redo, or selective restoration.

### Review and result boundaries

- Always show affected folder and session counts near the action. Require extra
  impact review when a change governs other observed folders, replaces a
  mapping to another project, or changes sessions spread across several
  effective projects.
- More specific mapped children remain protected by longest-prefix precedence.
  Mention them, but do not require extra review when their classifications
  will not change.
- Read-only stores retain the inventory, folder state, rule state, and impact
  information. Mutation actions are unavailable with a direct explanation that
  the current source is read-only.
- If persistence succeeds and the following inventory refresh fails, report the
  correction as saved and offer a refresh retry. A stale preview conflict
  makes no change and requires a fresh review.
