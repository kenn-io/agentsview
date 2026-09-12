# Define undo and change-history semantics

Type: grilling

Status: resolved

Blocked by: 02, 05

## Question

What must Undo restore after a project correction, how long should immediate
Undo remain available, and what durable history or reversal controls belong in
Project mapping rules? Reconcile the desired user promise with existing rule and
reclassification behavior before choosing the contract.

## Answer

Each saved project correction creates an archive-wide project mapping rule-set
version. Immediate Undo restores the preceding version and recomputes effective
classification for all current affected sessions. It does not replay stored
per-session values. Sessions added after the correction therefore receive the
classification produced by the restored rule set.

Undo remains available in the correction's success flash until the user
dismisses it, reloads the page, or successfully saves another project mapping
rule change. It is valid only while the correction's version remains active and
latest. If another rule change has succeeded on any machine, reject the stale
Undo without changing the archive and refresh the inventory.

Undo remains one action when only the number of affected sessions has changed.
If current impact includes an additional distinct folder path or current project
that was not part of the saved impact, replace Undo with the inline impact
review defined for project corrections. The review shows the current all-time
and 30-day counts and requires **Confirm and undo**.

Restoring the prior rule set, recomputing current session classifications, and
reconciling project identities form one transaction. If Undo removes the only
rule and sessions that refer to a project created by the correction, that empty
project disappears from inventory.

A committed correction whose inventory refresh failed offers both **Undo** and
**Retry refresh**. A failure before the Undo transaction commits reports
**Nothing was undone** and leaves the current version active. If Undo commits
but the following inventory refresh fails, report that Undo completed and offer
**Retry refresh**. Production feedback uses kit-ui's `FlashBanner` and
`showFlash` pattern.

The first production version offers one immediate Undo only. After Undo, it does
not offer another Undo or Redo. Project mapping rules do not yet expose a
history browser, repeated traversal, or selective restoration.

Retain rule-set versions without automatic pruning. Each version records its
timestamp, change kind, and one or more per-rule deltas. Each delta records its
machine, path prefix, target before and after, matched-session, changed-session,
30-day changed-session, changed-path, and affected-project counts at save time.
The version also retains the exact normalized folder identities and project
identities in its saved impact. Undo compares those identities with its current
impact to decide whether the scope has expanded. Saving after Undo creates a new
version from the restored version. Versions skipped by that new line remain
audit records.
