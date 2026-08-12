# VS Code Copilot Response Item Parsing

## Problem

VS Code 1.132 persists completed Copilot response items with shapes that the
current parser does not accept. `isConfirmed` can be an object instead of a
boolean, terminal commands can be nested below `toolSpecificData.commandLine`,
and file references are represented as `inlineReference` items between text
items.

The parser currently unmarshals each whole item into a struct with a boolean
`isConfirmed` field. An object value makes that unmarshal fail, so the parser
silently skips the entire tool invocation. It also deliberately skips inline
references, which joins the surrounding text without the referenced file name.

## Design

Keep the final `request.response` array as the authoritative ordered response.
Do not recover tool calls from `result.metadata.toolCallRounds`, because the
same calls appear in both representations and reconciliation would introduce a
duplicate-counting path.

Make fields that are not used to construct parsed output schema-tolerant so a
new representation cannot invalidate the rest of a response item. Preserve an
`inlineReference` item as inline-code text containing its file path. This keeps
the reference visible at its exact position without exposing a local-only
`file://` link that would not work for remote viewers.

Read terminal commands from both supported locations:

- `toolSpecificData.command` for existing session files.
- `toolSpecificData.commandLine.original` for VS Code 1.132 session files.

Keep existing tool-name normalization and display formatting. Populate the tool
call input with the recovered command so the structured tool call and its
rendered block both show the command.

## Data Flow

For each response item, decode its stable fields and retain flexible raw JSON
for fields whose producer shape varies. Text items append their value. Inline
references append a backtick-delimited file path. Tool invocation items create
one `ParsedToolCall`, extracting the invocation label and terminal command from
the supported shapes. The request then produces one assistant message with the
tool calls and ordered display text.

Malformed or unknown response items continue to be skipped. A malformed inline
reference contributes no text. A tool invocation without a tool ID contributes
no tool call. These match the parser's current error policy.

## Test Strategy

Add a hand-reduced JSONL fixture inside the parser test. Derive its values from
the supplied issue session, but include only the response shapes needed for the
regression:

- Object-valued `isConfirmed` on completed read and terminal tools.
- Two read tools and one terminal tool.
- A terminal command under `commandLine.original`.
- Inline references between assistant text fragments.

Assert the public parsed result: one assistant message has three structured tool
calls, the terminal call contains `uname -a`, and the assistant prose retains
the visible `test.txt` references. This test would fail if object-valued status
again invalidates item decoding, nested command extraction is removed, or file
references are discarded.

Update the VS Code Copilot session-format inventory with the observed 1.132
shapes and the issue artifact as implementation evidence.
