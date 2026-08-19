# Prototype project workspace variants

Type: prototype

Status: claimed

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

Use the scenario control to compare an ordinary move, broad impact review, and a
read-only source. Try saving a correction, creating a project name, and using
Undo. The ticket stays claimed until live review chooses a direction or asks for
another pass.
