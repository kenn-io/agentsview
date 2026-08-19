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

**Observed folder**:

A working directory recorded by one or more sessions and used as evidence for
project classification. Its identity is its machine and normalized path. It
remains relevant when the folder no longer exists because historical sessions
still refer to it.

**Project rule**:

A reusable, machine-specific instruction that classifies sessions under a folder
path as a project. When paths overlap, the longest matching path takes
precedence.

**Session assignment**:

An individual-session correction that does not create or change a reusable
project rule. It overrides project rules and inferred classification only for
that session.
