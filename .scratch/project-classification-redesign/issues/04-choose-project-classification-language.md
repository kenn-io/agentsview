# Choose project classification language

Type: grilling

Status: resolved

Blocked by: 02, 03

## Question

After seeing the terms in the prototypes, which user-facing language makes the
common workflow immediately understandable? Decide where to use phrases such as
"add folder," "move to project," "assign folders," "project rules," and
"mapping," and distinguish product copy from API and implementation terms.

## Answer

Frame the common workflow as a **project correction**. Use **Correct project**
for entry, **Project correction** for the editor, and **Save correction** for
commitment. Do not say that Agents View adds, moves, or assigns a folder: it
changes session classification and saves a durable rule; nothing moves on disk,
and assignment is a separate session-level concept.

Call the evidence-backed path groups **folder suggestions**. Introduce them as
"Paths suggested from sessions in this project." Use **Use for this project** as
the row action and **Path prefix** for the editable scope. The API term
`candidate` remains implementation language. Do not call these rows observed
folders because one row can combine several recorded working directories into a
synthesized prefix.

Call the durable machine-and-prefix policy a **project mapping rule**. The full
term belongs in the secondary administrative view and in explanations of what a
correction saves. The common correction flow should otherwise stay focused on
the user's intent. Avoid standalone **mapping**, broad **project rule**, and
implementation-facing **worktree mapping** in product copy.

Describe impact as classification change: for example, "74 sessions will change
project" and "2 folder paths affected." Never imply that a directory will move.
Use "No usable path prefix" for a suggestion whose evidence cannot produce a
safe prefix; this does not claim that the directory is missing from disk.
