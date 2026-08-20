# Prototype project workspace variants

Type: prototype

Status: resolved

Blocked by: 01

## Question

Which concrete project-detail design makes adding and moving folders quickest
while leaving the inventory overview calm? Build materially different rough
variants rather than variations of a preselected flow, put realistic project and
folder data through them, and compare them against the agreed tasks and
observable criteria through live human review.

## Comments

### Prototype ready for live review

The review artifact is
[project-classification-prototype.html](../../../frontend/project-classification-prototype.html).
From `frontend/`, run:

```sh
python3 -m http.server 4173 --bind 127.0.0.1
```

Open `http://127.0.0.1:4173/project-classification-prototype.html?variant=A`.
The bottom switcher and left and right arrow keys move between:

- A, Split inspector
- B, Inline ledger
- C, Focused workspace
- D, Folder triage

The first three answer this ticket. Folder triage is included only to support
the later live comparison in
[Prototype folder triage as a complementary workflow](03-prototype-folder-triage.md).
It does not resolve that ticket.

Use the scenario control to compare an ordinary move, broad impact review,
read-only source, stale preview, and saved-but-refresh-failed result. Try saving
a correction, creating a project name, and using Undo.

## Answer

Live review selected A, Split inspector, as the project-workspace structure.
Keep the full project inventory visible. Selecting a project opens a side pane
with its observed and suggested folders, correction controls, impact state, and
immediate Undo result.

The selection approves A's structure. No separate rationale was recorded, and it
does not approve the inline ledger or focused workspace. Folder triage remains a
separate decision in
[Prototype folder triage as a complementary workflow](03-prototype-folder-triage.md),
with Split inspector as its comparison baseline.

### Acceptance evidence

| Task or boundary                                                    | A: Split inspector                                                           | B: Inline ledger                                          | C: Focused workspace                         | D: Folder triage                       |
| ------------------------------------------------------------------- | ---------------------------------------------------------------------------- | --------------------------------------------------------- | -------------------------------------------- | -------------------------------------- |
| Keep inventory visible while selecting a project                    | Pass: inventory and inspector remain together                                | Pass, but expansion makes the inventory less calm         | Fail: dedicated workspace reduces overview   | Not applicable                         |
| Add or move a candidate with an explicit save                       | Pass: candidate, target, impact, review, and save stay in one pane           | Possible, but the correction is buried in an expanded row | Possible, but adds a third navigation column | Fail: begins from an unsupported queue |
| Require review when replacing a mapping or changing several folders | Pass: both ordinary replacement and broad-prefix cases gate save             | Uses the shared gate                                      | Uses the shared gate                         | Not applicable                         |
| Explain read-only and failure results                               | Pass: mutation controls disappear and conflict/refresh outcomes are explicit | Uses the shared result states                             | Uses the shared result states                | Not applicable                         |
| Recover the latest saved version                                    | Pass: one saved notice owns Undo and restores the full prior candidate state | Uses the shared Undo                                      | Uses the shared Undo                         | Not applicable                         |

A is the only accepted direction. B and C remain comparison artifacts, and D is
retained only as the rejected premise documented by ticket 03.
