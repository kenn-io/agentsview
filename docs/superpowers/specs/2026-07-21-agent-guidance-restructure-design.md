# Agent Guidance Restructure Design

## Problem

The root `AGENTS.md` has grown to cover four jobs:

- repository-wide rules;
- subsystem rules;
- project orientation;
- build and troubleshooting notes.

Codex loads the whole file for every task. Narrow rules therefore compete with
the rules that apply to all work. Repeated sections and copied reference text
also drift out of sync with the code and other documentation.

## Goal

Keep the root instructions short enough to scan as one policy document. Route
agents to focused guidance only when a task needs it. Preserve every safety and
delivery constraint that applies across the repository.

## Structure

The root `AGENTS.md` will contain:

1. scope and instruction precedence;
1. repository-wide safety rules;
1. git and delivery rules;
1. the definition of done and core validation commands;
1. a task-routing table;
1. pull request rules;
1. a short project map.

Focused files under `docs/agents/` will hold rules for:

- tests;
- storage and database changes;
- background work and memory investigations;
- build and dependency troubleshooting.

Frontend work will continue to use `frontend/AGENTS.md` and `DESIGN.md`. The
root routing table will tell agents to read them before changing frontend files.
This explicit route matters when Codex starts at the repository root, because it
does not discover a nested instruction file based only on a file later edited
during the task.

Normal project facts will stay in `README.md`, package READMEs, and the
`Makefile`. The root file may point to those sources but will not copy their
full contents.

## Rule Placement

Keep a rule in the root file when missing it could damage user data, publish
private information, break the required delivery process, or affect most
changes.

Move a rule to a focused file when it applies only to a subsystem or task type.
Each route in the root file will name the paths that trigger the focused guide.

Move facts and command catalogues to normal documentation when an agent can look
them up without changing how it must work.

## Editing Standard

Use plain English throughout:

- cut words that do not change meaning;
- prefer active voice and concrete nouns;
- use short words where they remain exact;
- avoid stock introductions, summaries, and repeated points;
- keep technical terms when replacing them would weaken the rule.

## Commit Boundaries

Each commit will leave the guidance usable:

1. record this design;
1. record the implementation plan;
1. add focused agent guides;
1. slim the root file and add routes to those guides;
1. fix stale documentation found while moving the rules.

These boundaries let maintainers revert one part without undoing the rest.

## Verification

The final check will confirm that:

- `CLAUDE.md` remains a symlink to `AGENTS.md`;
- every routed file exists;
- the root file contains no duplicate headings;
- the root and focused guides preserve the current safety, storage, testing,
  validation, and delivery rules;
- Markdown formatting passes;
- no private data or absolute user path enters tracked content.
