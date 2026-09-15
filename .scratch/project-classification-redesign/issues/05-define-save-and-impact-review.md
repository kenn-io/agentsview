# Define save and impact-review behavior

Type: grilling

Status: resolved

Blocked by: 02

## Question

What should happen between choosing a folder and seeing the updated project?
Define the one-action normal path, the conditions that require an impact review,
the information shown in that review, conflict handling, and feedback after
persistence and archive reclassification. Specify interruption and partial-
failure behavior without promising atomicity beyond the verified storage
boundary.

## Answer

Selecting **Use for this project** opens an inline project correction with the
suggested path prefix, target project, and current impact. An ordinary
correction needs one explicit **Save correction** action and no additional
confirmation.

Replace the save action with **Review impact** when the correction replaces an
existing project mapping rule, changes sessions from more than one current
project, or changes sessions from more than one distinct folder path. Keep the
review in the inspector. It has **Back** and **Confirm and save** actions rather
than a modal.

Impact includes all matching sessions, all sessions changing project, the
changing sessions active within the preceding 30 days, affected current
projects, representative examples, and distinct folder paths changing project.
Each expanded path shows its all-time and 30-day session counts. A changed path
is a distinct normalized working-directory path belonging to at least one
session whose effective classification will change. Do not count unchanged
sessions or sessions protected by a more specific rule or session assignment.
The 30-day window is fixed initially; a later design may add a picker without
changing the all-time counts that describe the full saved effect.

Allow a save with zero current classification changes when it creates or changes
a rule that will classify future matching sessions. Explain that future effect
directly. Disable save when the proposed rule and effective policy are
unchanged.

Any edit after preview invalidates it. If the archive changes before apply,
refresh impact automatically, explain that nothing was saved, and require the
user to review or save again. Preserve the proposed correction. Other failures
before commit also preserve the correction and state that nothing was saved.

While saving, disable correction controls and prevent the inspector from being
closed. Do not trap browser or route navigation. The request may still finish
after the user leaves; the writable archive remains authoritative when they
return.

For SQLite, creating or updating the project mapping rule, changing all affected
session classifications, and reconciling project identities share one
transaction. A failure before commit rolls back that transaction. Events, the
HTTP response, and inventory refresh occur after commit. If refresh fails, say
that the correction was saved and offer **Retry refresh** without another save
action. Production success and failure feedback should use kit-ui's
`FlashBanner` and `showFlash` pattern.

Give every apply request a client-generated correction ID and store that ID with
the rule-set version in the transaction. If the client loses the response, show
an indeterminate result instead of claiming success or failure. Reconcile by
reading the authoritative result for that correction ID. If it committed, show
the saved state and refresh the inventory. If it did not commit, restore the
editor and allow a retry with the same ID. Repeated apply requests with one
correction ID must return the original result without creating another rule-set
version.
