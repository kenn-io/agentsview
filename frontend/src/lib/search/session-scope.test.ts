// @vitest-environment jsdom
import { describe, expect, it } from "vite-plus/test";
import type { Message } from "../api/types.js";
import type { BlockType } from "../stores/ui.svelte.js";
import { keepsAnswerBeforeTrailingTools, projectSessionScope } from "./session-scope.js";

const ALL: ReadonlySet<BlockType> = new Set([
  "user",
  "assistant",
  "thinking",
  "tool",
  "code",
  "system",
]);

let nextId = 970000;
function message(ordinal: number, content: string, overrides: Partial<Message> = {}): Message {
  return {
    id: nextId++,
    session_id: "scope",
    ordinal,
    role: "assistant",
    content,
    content_length: content.length,
    timestamp: "2026-01-01T00:00:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
    ...overrides,
  };
}

function scope(
  messages: Message[],
  overrides: Partial<Parameters<typeof projectSessionScope>[0]> = {},
) {
  return projectSessionScope({
    messages,
    transcriptMode: "normal",
    visibleBlocks: ALL,
    hasBlockFilters: false,
    ...overrides,
  });
}

function filtered(visible: BlockType[]) {
  return { visibleBlocks: new Set(visible), hasBlockFilters: visible.length < ALL.size };
}

describe("session search scope", () => {
  it("keeps visible prose in a mixed message and drops its hidden tool blocks", () => {
    const mixed = message(1, "needle prose\n\n[Bash]\necho needle", { has_tool_use: true });
    const result = scope([mixed], filtered(["user", "assistant"]));
    expect(result.messages).toContain(mixed);
    expect(result.allowsBlock(mixed, "text")).toBe(true);
    expect(result.allowsBlock(mixed, "tool-input")).toBe(false);
    expect(result.allowsBlock(mixed, "tool-output")).toBe(false);
    expect(result.allowsBlock(mixed, "tool-history")).toBe(false);
  });

  it("keeps thinking in a message whose tools are hidden and drops legacy tool-only rows", () => {
    const thinkingPlusTool = message(2, "[Thinking]\nneedle\n[/Thinking]", {
      has_thinking: true,
      has_tool_use: true,
      tool_calls: [{ tool_name: "Bash" }],
    });
    const legacyTool = message(3, "[Bash]\necho needle", { has_tool_use: true });
    const result = scope(
      [thinkingPlusTool, legacyTool],
      filtered(["user", "assistant", "thinking"]),
    );
    expect(result.messages).toContain(thinkingPlusTool);
    expect(result.allowsBlock(thinkingPlusTool, "thinking")).toBe(true);
    expect(result.allowsBlock(thinkingPlusTool, "tool-output")).toBe(false);
    expect(result.messages).not.toContain(legacyTool);
    expect(result.allowsBlock(legacyTool, "tool-input")).toBe(false);
  });

  it("maps text and skill blocks to the message role filter", () => {
    const user = message(4, "hello", { role: "user" });
    const assistant = message(5, "hello");
    const result = scope([user, assistant], filtered(["assistant"]));
    expect(result.messages).not.toContain(user);
    expect(result.allowsBlock(user, "text")).toBe(false);
    expect(result.allowsBlock(user, "skill")).toBe(false);
    expect(result.messages).toContain(assistant);
    expect(result.allowsBlock(assistant, "text")).toBe(true);
    expect(result.allowsBlock(assistant, "skill")).toBe(true);
  });

  it("maps thinking and code to their own filters", () => {
    const both = message(6, "[Thinking]\nthink\n[/Thinking]\n\n```ts\nconst x = 1;\n```");
    const result = scope([both], filtered(["user", "assistant", "thinking"]));
    expect(result.allowsBlock(both, "thinking")).toBe(true);
    expect(result.allowsBlock(both, "code")).toBe(false);
    expect(result.allowsBlock(both, "text")).toBe(true);
  });

  it("focused mode narrows the scope to the rendered turns", () => {
    const messages = [
      message(0, "prompt", { role: "user" }),
      message(1, "intermediate"),
      message(2, "final"),
      message(3, "next", { role: "user" }),
    ];
    const result = scope(messages, { transcriptMode: "focused" });
    expect(result.items.flatMap((item) => item.ordinals)).toEqual([0, 2, 3]);
    expect(result.messages.map((item) => item.ordinal)).toEqual([0, 2, 3]);
    expect(result.allowsBlock(messages[1]!, "text")).toBe(false);
    expect(result.normalItems.flatMap((item) => item.ordinals)).toEqual([0, 1, 2, 3]);
  });

  it("keeps the answer before trailing tools only when the provider asks", () => {
    const messages = [
      message(0, "prompt", { role: "user" }),
      message(1, "answer"),
      message(2, "[Bash]\necho done", { has_tool_use: true }),
    ];
    const dropped = scope(messages, { transcriptMode: "focused" });
    expect(dropped.messages.map((item) => item.ordinal)).toEqual([0]);
    const kept = scope(messages, {
      transcriptMode: "focused",
      keepAnswerBeforeTrailingTools: true,
    });
    expect(kept.messages.map((item) => item.ordinal)).toEqual([0, 1]);
  });

  it("keeps system rows out of the searchable scope", () => {
    const system = message(7, "needle", { is_system: true });
    const result = scope([system]);
    expect(result.messages).toHaveLength(0);
    expect(result.allowsBlock(system, "text")).toBe(false);
  });

  it("resolves the provider trailing-tool preference by agent id", () => {
    const providers = [
      { id: "claude", post_answer_tool_work: true },
      { id: "codex", post_answer_tool_work: false },
    ];
    expect(keepsAnswerBeforeTrailingTools(providers, "claude")).toBe(true);
    expect(keepsAnswerBeforeTrailingTools(providers, "codex")).toBe(false);
    expect(keepsAnswerBeforeTrailingTools(providers, "other")).toBe(false);
    expect(keepsAnswerBeforeTrailingTools(providers, undefined)).toBe(false);
  });
});
