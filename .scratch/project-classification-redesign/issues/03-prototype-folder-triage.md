# Prototype folder triage as a complementary workflow

Type: prototype

Status: resolved

Blocked by: 01

## Question

Does a folder-first triage workspace materially improve cleanup of many
misclassified projects, and if so, should it be a complementary entry point
rather than the everyday Data experience? Build a rough artifact and resolve the
ticket through live human comparison with the project workspace.

## Answer

Live review rejected folder-first triage. The prototype could not explain how a
folder enters a queue such as "Needs attention" because the product has no such
domain state or derived classification.

The current project inventory contains project-level counts and rule
attribution. The folder-candidate API starts from a project the user already
selected and returns a machine, suggested prefix, evidence kind, counts, and
lexical availability. It does not return an archive-wide folder inventory,
current-versus-inferred project mismatch, missing-path result, or problem state.

The prototype's
[Folder triage variant](../../../frontend/project-classification-prototype.html?variant=D)
hard-coded `project`, `inferred`, and `status` values, then built its queues
from those invented values. Its "Missing" state was also unsupported because
candidate availability does not inspect the filesystem.

Do not add a folder-triage entry point or a hidden classifier to support one.
Keep corrections deliberate and project-first in Split inspector. A future
triage effort would first need a separately approved, evidence-based definition
of queue membership.
