import type { DbMessage as Message } from "../api/generated/index.js";

export function msg(overrides: Partial<Message>): Message {
  return {
    has_context_tokens: false,
    has_output_tokens: false,
    id: 1,
    session_id: "s1",
    ordinal: 0,
    role: "user",
    content: "hello",
    timestamp: "2025-01-01T00:00:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: 5,
    model: "",
    token_usage: null,
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
    ...overrides,
  };
}
