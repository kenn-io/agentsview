# Grok Provider Documentation Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development
> (if subagents available) or superpowers:executing-plans to implement this
> plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Grok discoverable in every canonical public provider list and
document its configuration and summary-only transcript fidelity.

**Architecture:** Update the existing README and Zensical documentation sources
in place. Keep provider names, paths, configuration fields, and fidelity
language identical across the public references, with no product-code or
localization changes.

**Tech Stack:** Markdown, Zensical documentation source checks

______________________________________________________________________

### Task 1: Document the Grok provider

**Files:**

- Modify: `README.md`

- Modify: `docs/configuration.md`

- Modify: `docs/quickstart.md`

- Modify: `docs/commands.md`

- Modify: `docs/index.md`

- [x] **Step 1: Update the README supported-agent reference**

Add Grok to the alphabetical supported-agent table with the default directory
`~/.grok/sessions/`. Add a short Grok note after the table explaining that
AgentsView reads the summary, searchable first prompt, timestamps, project
label, and message count from `summary.json`; reads total output tokens and peak
context tokens from `signals.json` when present; and does not currently decode
the full transcript from `updates.jsonl` or `chat_history.jsonl`. Name
`GROK_DIR` and `grok_dirs` as the directory overrides.

- [x] **Step 2: Update the configuration reference**

Add Grok to the alphabetical session-discovery table with default directory
`~/.grok/sessions/` and file format `summary.json` metadata plus optional
`signals.json` token counters. Add a matching summary-only fidelity note near
the discovery table that explicitly names the summary, searchable first prompt,
timestamps, project label, message count, total output tokens, and peak context
tokens. Add `grok_dirs` to the complete alphabetical list of directory-array
configuration fields. Add `GROK_DIR` to the full single-directory
environment-variable example after `GPTME_DIR`.

- [x] **Step 3: Update the canonical environment-variable references**

Add `GROK_DIR` after `GPTME_DIR` in the quickstart's custom session-directory
example and the CLI reference's environment-variable table. Document the
default as `~/.grok/sessions` and describe it as the Grok sessions directory.

- [x] **Step 4: Update the documentation homepage**

Add a Grok chip to the provider grid in `docs/index.md`. Use the existing
monogram chip style, `data-agent="grok"`, label `Grok`, and link to
`/configuration/#session-discovery`.

- [x] **Step 5: Verify the documentation content**

Run:

```bash
rg -n 'Grok|GROK_DIR|grok_dirs|\.grok/sessions|summary-only|summary mode' \
  README.md docs/configuration.md docs/quickstart.md docs/commands.md \
  docs/index.md
git diff --check
make docs-check
```

Expected: Grok appears in all five public documentation files; the README and
configuration guide contain the default path, both override names, and an
accurate summary-only fidelity note; the configuration, quickstart, and command
references contain the canonical `GROK_DIR` entry; `git diff --check` and
`make docs-check` exit 0.

- [x] **Step 6: Commit the documentation update**

Review the full diff, stage only the five public documentation files and two
process artifacts, and commit with a conventional `docs:` subject and a
rationale-focused body. Follow the repo's mandatory commit workflow and omit
generated-with attribution as required by `AGENTS.md`.
