import { describe, expect, it } from "vite-plus/test";
import type { DbMessage } from "../api/generated/index.js";
import { exactMessageIdentity, messageIdentity, resolveMessageIdentity } from "./message-identity.js";

const message = (ordinal: number, content: string): DbMessage => ({
  id: ordinal, session_id: "session", ordinal, role: "assistant", content, content_length: content.length,
  model: "", timestamp: "2026-01-01T00:00:00Z", has_thinking: false, thinking_text: "", has_tool_use: false,
  context_tokens: 0, output_tokens: 0, has_context_tokens: false, has_output_tokens: false, is_system: false,
});

describe("message identity", () => {
  it("finds a message after earlier messages shift its ordinal", () => {
    const original = message(1, "answer");
    const identity = messageIdentity(original);
    expect(resolveMessageIdentity([message(1, "inserted"), { ...original, ordinal: 2, id: 99 }], identity)).toBe(2);
  });

  it("uses an unchanged row without reading older pages", () => {
    const original = message(3, "answer");
    expect(exactMessageIdentity([original], messageIdentity(original), 3)).toBe(3);
    expect(exactMessageIdentity([{ ...original, ordinal: 4 }], messageIdentity(original), 3)).toBeNull();
    expect(exactMessageIdentity([{ ...original, content: "replacement" }], messageIdentity(original), 3)).toBeNull();
  });

  it("uses a unique source UUID after the row and body change", () => {
    const original = { ...message(1, "answer"), source_uuid: "source-1" };
    expect(resolveMessageIdentity([{ ...message(9, "updated answer"), source_uuid: "source-1" }], messageIdentity(original))).toBe(9);
  });

  it("refuses a duplicated source when its fingerprint also matches twice", () => {
    const original = { ...message(1, "answer"), source_uuid: "source-1" };
    expect(resolveMessageIdentity([{ ...original, id: 8 }, { ...original, id: 9, ordinal: 2 }], messageIdentity(original))).toBeNull();
  });

  it("refuses a reused row ID with a different message", () => {
    expect(resolveMessageIdentity([message(1, "replacement")], messageIdentity(message(1, "answer")))).toBeNull();
  });

  it("refuses missing and ambiguous matches", () => {
    const original = message(1, "answer");
    const identity = messageIdentity(original);
    expect(resolveMessageIdentity([message(1, "different")], identity)).toBeNull();
    expect(resolveMessageIdentity([original, { ...original, ordinal: 2 }], identity)).toBeNull();
  });
});
