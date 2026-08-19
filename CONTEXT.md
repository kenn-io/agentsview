# Agents View

Agents View organizes local AI-agent sessions so developers can inspect their
work across projects and tools.

## Language

**Project**:

A named grouping of sessions that belong to the same codebase.

**Project classification**:

The association between a session and its project. Classification may be
inferred from session evidence, governed by a project rule, or set by a session
assignment. A session assignment takes precedence over a project rule, and a
project rule takes precedence over inferred classification.

**Inferred classification**:

A session's project classification derived from session evidence before project
rules or session assignments apply.

**Effective classification**:

A session's project classification after precedence rules apply. It can be
reconstructed from inferred classification, the active project rule set, and any
session assignment.

**Observed folder**:

A working directory recorded by one or more sessions and used as evidence for
project classification. Its identity is its machine and normalized path. It
remains relevant when the folder no longer exists because historical sessions
still refer to it, and it may still receive a project rule. A renamed path is a
different observed folder.

**Mapped folder**:

An observed folder for which a project rule produces a project classification.

**Unmapped folder**:

An observed folder for which no project rule produces a project classification.
Session classifications do not map the folder. An unmapped folder may contain
mapped child folders.

**Project rule**:

A reusable, machine-specific instruction that classifies sessions under a folder
path as a project. When paths overlap, the longest matching rule that produces a
project classification takes precedence.

**Project rule set**:

The archive-wide, versioned collection of project rules. Undo walks backward
through its linear history one version at a time; a later save continues from
the restored version while skipped versions remain audit records.

**Session assignment**:

An individual-session correction that does not create or change a reusable
project rule. It overrides project rules and inferred classification only for
that session.
