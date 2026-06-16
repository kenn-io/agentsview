# QwenPaw Test Fixtures

Synthetic, sanitized QwenPaw session files for agentsview regression
testing. Covers the four on-disk layouts the parser must handle:

| Fixture                                                  | Layout                  | What it exercises                                   |
| -------------------------------------------------------- | ----------------------- | --------------------------------------------------- |
| `default/sessions/default_1700000000000.json`            | root sessions/          | basic user/assistant exchange + thinking block      |
| `default/sessions/main_main.json`                        | root sessions/          | tool_use + tool_result round-trip via system role   |
| `default/sessions/console/default_1700000000001.json`    | sessions/console/       | subdir layout, exercises the ID collision fix       |
| `note_keeper/sessions/user@example.com_1700000000002.json` | root sessions/        | channel-scoped filename (`@`, dots) — ID validation |
| `researcher/sessions/empty.json`                         | root sessions/          | empty `agent.memory.content` edge case              |

All user identifiers, agent names, file paths, and tool call IDs are
synthetic. No PII, no live tokens, no real session UUIDs.

## Regenerating

The fixtures are produced by `gen.py`, which hand-authors synthetic
QwenPaw session payloads that match the on-disk shape documented in
QwenPaw's `session.py`. The output is pretty-printed (`indent=2`)
for readability — it is **not** byte-identical to a live runtime's
compact JSON, but the parser is whitespace-insensitive so this is
fine for fixtures. From the agentsview repo root:

```bash
python3 scripts/qwenpaw-fixtures/gen.py \
    --qwenpaw-src ~/develop/QwenPaw \
    --out internal/parser/testdata
```

Requirements: QwenPaw source tree checked out locally. The script
imports `qwenpaw.app.runner.session` to sanity-check the source path
but writes fixtures directly (no QwenPaw runtime needed).

## Source-of-Truth Reference

The on-disk shape is defined in QwenPaw itself:

- **File**: `QwenPaw/src/qwenpaw/app/runner/session.py`
- **Serializer**: `SessionManager.save_session_state` (~line 314)
- **Path derivation**: `SessionManager._get_save_path` (~line 258)
- **Atomic write**: module-level `_atomic_write_json`

Shape (per `state_dicts` written by `save_session_state`):

```jsonc
{
  "agent": {
    "memory": {
      "content": [[message, []], [message, []], ...]
    },
    "toolkit": {"active_groups": []},
    "name": "<AgentName>",
    "_sys_prompt": "..."
  }
}
```

Each `message` is an Anthropic-style content block envelope:

```jsonc
{
  "id":        "msg_..." | "<short-alphanum>",
  "name":      "user" | "<AgentName>" | "system",
  "role":      "user" | "assistant" | "system",
  "content":   [text|thinking|tool_use|tool_result],
  "metadata":  {},
  "timestamp": "YYYY-MM-DD HH:MM:SS.fff"   // local time, ms
}
```

System-role messages carry `tool_result` blocks (QwenPaw's equivalent
of Anthropic's user-side tool_result).
