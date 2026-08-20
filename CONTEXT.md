# Agents View

Agents View organizes local AI-agent sessions so developers can inspect their
work across projects and tools.

## Language

**Project**:

A named grouping of sessions that belong to the same codebase.

**Project classification**:

The association between a session and its project. Classification may be
inferred from session evidence, governed by a project mapping rule, or set by a
session assignment. A session assignment takes precedence over a project mapping
rule, which takes precedence over inferred classification.

**Inferred classification**:

A session's project classification derived from session evidence before project
mapping rules or session assignments apply.

**Effective classification**:

A session's project classification after precedence rules apply. It can be
reconstructed from inferred classification, the active project mapping rule set,
and any session assignment.

**Observed folder**:

A working directory recorded by one or more sessions. Its identity is its source
archive, machine, and normalized path; a renamed path is a different observed
folder.

**Folder suggestion**:

An evidence-backed path prefix offered within a selected project as the starting
point for a project correction. It is not a persistent folder state or an
archive-wide problem signal. _Avoid_: Candidate, suggested folder

**Project correction**:

A user-initiated change to project classification for sessions under a path
prefix. It can create or change a project mapping rule, but it never moves a
directory on disk. _Avoid_: Add folder, move folder, assign folder

**Mapped folder**:

An observed folder for which a project mapping rule produces a project
classification.

**Unmapped folder**:

An observed folder for which no project mapping rule produces a project
classification. An unmapped folder may contain mapped child folders.

**Project mapping rule**:

A reusable, machine-specific instruction that classifies sessions under a folder
path as a project. When paths overlap, the longest matching rule that produces a
project classification takes precedence. _Avoid_: Project rule, mapping,
worktree mapping

**Project mapping rule set**:

The archive-wide, versioned collection of project mapping rules. Undo walks
backward through its linear history one version at a time.

**Session assignment**:

An individual-session correction that does not create or change a reusable
project mapping rule. It overrides project mapping rules and inferred
classification only for that session.
