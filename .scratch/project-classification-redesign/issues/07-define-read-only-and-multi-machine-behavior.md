# Define read-only and multi-machine behavior

Type: grilling

Status: resolved

Blocked by: 02

## Question

How should the project workflow show machine scope, folders from several
machines, and read-only archives without exposing administrative details in the
common case? Decide what remains inspectable, how the writable source is
identified, and where the user is sent to make a correction.

## Answer

Call the Agents View instance that owns session ingestion and project
corrections the **Source Agents View**. A read-only mirror keeps the project
inventory, folder suggestions, project mapping rule attribution,
governed-session counts, and evidence examples. It removes **Use for this
project**, correction fields, and save actions instead of showing an editor that
cannot persist.

Each Source Agents View publishes its stable source identity and a
human-readable display name to its mirrors. Do not show the opaque identity. The
first version does not publish or link to a source URL because the source is
often unreachable from the mirror. Explain the boundary directly, for example:
"This is a read-only copy from Dev Mac. Make corrections in that Source Agents
View."

Folder suggestions and project mapping rules in a mirror retain their source
identity. Candidate identity and grouping include the source before machine and
path evidence so two sources cannot merge because they use the same machine name
and path. When several sources contribute to one project, group rows first by
Source Agents View and then by machine. When only one source contributes, omit
its heading. Within a source, add machine headings only when several machines
contribute; otherwise keep the machine as quiet row metadata.

An ordinary project correction creates or changes one machine-specific rule. The
same path on another machine remains unchanged. Offer **Copy to all machines**
as a deliberate bulk action for users who want the exact path prefix and target
everywhere. "All" means every machine in the current Source Agents View that has
current sessions or saved project mapping rules. An identical rule is a no-op
and is skipped. A machine with no current matching sessions may still receive
the rule for future sessions.

Copying to all machines opens one combined impact review before persistence.
List every machine, replacements, all-time and 30-day impact, and zero-match
rules. Save all required machine-specific rules in one transaction and one
project mapping rule-set version. Any conflict or failure rejects the entire
bulk change. Immediate Undo restores the whole multi-machine version as one
change.
