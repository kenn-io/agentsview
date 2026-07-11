# Grok Provider Documentation Design

## Context

The Grok provider is registered as a supported session source, defaults to
`~/.grok/sessions/`, and can be overridden with `GROK_DIR` or `grok_dirs`. Its
initial implementation did not update the public provider lists in the README or
documentation site.

Grok support is metadata-first. AgentsView reads `summary.json` for the session
summary, first prompt, timestamps, project label, and message count. It also
reads token counters from `signals.json` when present. It does not decode the
full conversation from `updates.jsonl` or `chat_history.jsonl`; only the first
prompt is available as searchable message content.

## Approach

Update the existing canonical provider references rather than add a dedicated
Grok page:

- Add Grok and its default directory to the README supported-agent table.
- Add Grok, its default directory, and its metadata files to the configuration
  guide's session-discovery table.
- Add `grok_dirs` to the configuration guide's complete list of directory
  override fields.
- Add `GROK_DIR` to the configuration guide's full single-directory environment
  variable example, the quickstart's custom session-directory example, and the
  CLI reference's environment-variable table.
- Add Grok to the documentation homepage provider grid, linking to the
  session-discovery reference.
- Add a short note near the provider tables explaining the metadata-first
  fidelity boundary and the available session and token fields.

This keeps Grok discoverable everywhere other supported providers are listed
while avoiding a standalone page for a small, stable configuration surface.

## Validation

Run `make docs-check` and inspect the rendered Markdown source for consistent
table formatting, links, and terminology. Confirm that Grok appears in
`README.md`, `docs/configuration.md`, `docs/quickstart.md`, `docs/commands.md`,
and `docs/index.md`, and that the public documentation covers `GROK_DIR`,
`grok_dirs`, `~/.grok/sessions/`, and the summary-only transcript fidelity.
Confirm that `GROK_DIR` appears in the configuration guide's full environment
variable example, the quickstart's custom session-directory example, and the
CLI reference's environment-variable table. No product code or localized UI
copy changes are required.
