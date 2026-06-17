# Visual Studio Copilot Trace Format

Visual Studio Copilot trace data is not the same format as VS Code Copilot chat
sessions.

VS Code Copilot stores app-level chat session documents. Visual Studio Copilot
stores OpenTelemetry-style trace JSONL, where each JSONL line is a trace
envelope containing spans.

## VS Code Copilot Format

VS Code Copilot session files are usually found under paths like:

```text
workspaceStorage/<hash>/chatSessions/<uuid>.json
workspaceStorage/<hash>/chatSessions/<uuid>.jsonl
globalStorage/emptyWindowChatSessions/<uuid>.json
globalStorage/transferredChatSessions/<uuid>.json
```

The top-level shape is a chat session object:

- Session ID is usually `sessionId`.
- Turns are stored in `requests[]`.
- User prompts are stored in `requests[].message.text`.
- Assistant and tool output are stored in `requests[].response[]`.
- One file generally represents one chat session.

Sanitized shape:

```json
{
  "version": 3,
  "sessionId": "<uuid>",
  "creationDate": 1781293600000,
  "lastMessageDate": 1781293610000,
  "requests": [
    {
      "requestId": "<uuid>",
      "message": {
        "text": "<user prompt>"
      },
      "response": [
        {
          "kind": "textEditGroup",
          "value": "<assistant-or-tool-output>"
        }
      ],
      "modelId": "<model>",
      "timestamp": 1781293600000
    }
  ]
}
```

## Visual Studio Copilot Format

Visual Studio Copilot trace files are usually found under:

```text
%LOCALAPPDATA%\Temp\VSGitHubCopilotLogs\traces\*_VSGitHubCopilot_traces.jsonl
```

Each line is an OpenTelemetry JSON envelope:

- `resourceSpans[]`
- `scopeSpans[]`
- `spans[]`
- `attributes[]`

Session data is reconstructed from span attributes:

- Conversation ID is `gen_ai.conversation.id`.
- Chat input is `gen_ai.input.messages`.
- Chat output is `gen_ai.output.messages`.
- Tool calls use `gen_ai.tool.name`, `gen_ai.tool.call.id`, and
  `gen_ai.tool.call.arguments`.
- Tool results use `gen_ai.tool.call.result`.
- Token usage uses `gen_ai.usage.input_tokens` and `gen_ai.usage.output_tokens`.
- Multiple trace files can contain spans for the same conversation, so the
  parser stitches them together by `gen_ai.conversation.id`.

## Sanitized Visual Studio Examples

These examples preserve the structure of real Visual Studio Copilot trace JSONL
spans while redacting prompts, paths, commands, patches, IDs, and results.

### Chat Span

```json
{
  "sourceFile": "20260615T234102_b45c44b2_VSGitHubCopilot_traces.jsonl",
  "traceId": "<trace-id>",
  "spanId": "<span-id>",
  "name": "chat gpt-5.5",
  "startTimeUnixNano": "<unix-nano>",
  "endTimeUnixNano": "<unix-nano>",
  "attributes": {
    "virtual_parent.genai": "905ec1a433bff543",
    "gen_ai.provider.name": "github",
    "gen_ai.request.model": "<model>",
    "gen_ai.response.model": "<model>",
    "gen_ai.response.id": "<redacted>",
    "gen_ai.response.finish_reasons": "",
    "gen_ai.usage.input_tokens": "94179",
    "gen_ai.usage.output_tokens": "218",
    "gen_ai.conversation.id": "<uuid-or-call-id>",
    "copilot_chat.client_id": "Microsoft.VisualStudio.Conversations.Chat.HelpWindow",
    "copilot_chat.root_request_id": "<uuid-or-call-id>",
    "server.address": "api.githubcopilot.com",
    "gen_ai.tool.definitions": "<redacted>",
    "gen_ai.input.messages": "[{\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"content\":\"<user prompt>\"}]}]",
    "gen_ai.output.messages": "[{\"role\":\"assistant\",\"parts\":[{\"type\":\"tool_call\",\"id\":\"<call-id>\",\"name\":\"<tool-name>\",\"arguments\":\"<json-encoded-arguments>\"},{\"type\":\"text\",\"content\":\"<assistant text>\"}]}]",
    "gen_ai.operation.name": "chat"
  }
}
```

### Tool Execution Span

```json
{
  "sourceFile": "20260615T234059_b45c44b2_VSGitHubCopilot_traces.jsonl",
  "traceId": "<trace-id>",
  "spanId": "<span-id>",
  "name": "execute_tool get_file",
  "startTimeUnixNano": "<unix-nano>",
  "endTimeUnixNano": "<unix-nano>",
  "attributes": {
    "virtual_parent.genai": "26401e75c1171a63",
    "gen_ai.conversation.id": "<uuid-or-call-id>",
    "gen_ai.tool.call.result": "{\"Value\":\"<tool result>\"}",
    "gen_ai.provider.name": "other",
    "gen_ai.tool.name": "get_file",
    "gen_ai.tool.call.id": "<uuid-or-call-id>",
    "gen_ai.tool.type": "extension",
    "gen_ai.tool.description": "<redacted>",
    "gen_ai.tool.call.arguments": "{\"filename\":\"<path>\",\"command\":\"<command>\",\"patch\":\"<patch>\"}",
    "gen_ai.operation.name": "execute_tool"
  }
}
```

### Invoke-Agent Span

```json
{
  "sourceFile": "20260615T234059_b45c44b2_VSGitHubCopilot_traces.jsonl",
  "traceId": "<trace-id>",
  "spanId": "<span-id>",
  "name": "invoke_agent GitHub Copilot",
  "startTimeUnixNano": "<unix-nano>",
  "endTimeUnixNano": "<unix-nano>",
  "attributes": {
    "gen_ai.provider.name": "other",
    "gen_ai.agent.name": "GitHub Copilot",
    "gen_ai.conversation.id": "<uuid-or-call-id>",
    "gen_ai.request.model": "<model>",
    "copilot_chat.turn_count": "1",
    "copilot_chat.client_id": "Microsoft.VisualStudio.Conversations.Chat.HelpWindow",
    "copilot_chat.entry_point": "Microsoft.VisualStudio.Copilot.AgentModeResponder",
    "copilot_chat.mode": "Agent",
    "copilot_chat.initiator_type": "User",
    "copilot_chat.root_request_id": "<uuid-or-call-id>",
    "gen_ai.operation.name": "invoke_agent"
  }
}
```

## Parser Implications

The Visual Studio parser treats the trace data as telemetry that must be
reconstructed into a transcript:

- It groups spans by `gen_ai.conversation.id`.
- It reads user turns from the stringified `gen_ai.input.messages` payload.
- It reads assistant text and tool-call proposals from the stringified
  `gen_ai.output.messages` payload.
- It reads executed tool calls and tool results from separate `execute_tool`
  spans.
- It de-duplicates repeated prompt snapshots and duplicate chat/execute tool
  records for the same tool call ID.
- It stitches sibling trace files because a single conversation can be split
  across multiple `*_VSGitHubCopilot_traces.jsonl` files.
