// @vitest-environment jsdom
import { describe, expect, it } from "vite-plus/test";
import type { DbMessage as Message } from "../api/generated/index.js";
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
    has_context_tokens: false,
    has_output_tokens: false,
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
  it("reveals a linked system message without revealing other system messages", () => {
    const prompt = message(0, "prompt", { role: "user" });
    const target = message(1, "linked system message", { is_system: true, role: "user" });
    const other = message(2, "other system message", { is_system: true, role: "user" });
    const answer = message(3, "answer");
    const result = scope([prompt, target, other, answer], { revealOrdinal: 1 });
    expect(result.revealedOrdinal).toBe(1);
    expect(result.items.flatMap((item) => item.ordinals)).toContain(1);
    expect(result.items.flatMap((item) => item.ordinals)).not.toContain(2);
    expect(result.allowsBlock(target, "text")).toBe(true);
    expect(result.allowsBlock(other, "text")).toBe(false);
  });

  it("reveals a linked answer when thinking keeps its row visible", () => {
    const answer = message(1, "[Thinking]\nplan\n[/Thinking]\nlinked answer", {
      has_thinking: true,
    });
    const result = scope([answer], { ...filtered(["thinking"]), revealOrdinal: 1 });
    expect(result.revealedOrdinal).toBe(1);
    expect(result.allowsBlock(answer, "text")).toBe(true);
  });

  it("keeps hidden thinking out of search for a linked visible tool group", () => {
    const tool = (ordinal: number) =>
      message(ordinal, "[Thinking]\nprivate thought\n[/Thinking]", {
        has_thinking: true,
        has_tool_use: true,
        tool_calls: [{ call_index: 0, category: "", tool_name: "Read" }],
      });
    const linked = tool(1);
    const other = tool(2);
    const result = scope([linked, other], { ...filtered(["tool"]), revealOrdinal: 1 });
    expect(result.items.map((item) => [item.kind, item.ordinals])).toEqual([
      ["tool-group", [1, 2]],
    ]);
    expect(result.allowsBlock(linked, "tool-input")).toBe(true);
    expect(result.allowsBlock(linked, "thinking")).toBe(false);
  });

  it("reveals a hidden linked message without displacing a focused answer", () => {
    const prompt = message(0, "prompt", { role: "user" });
    const answer = message(1, "answer");
    const tool = message(2, "", {
      has_tool_use: true,
      tool_calls: [{ call_index: 0, category: "", tool_name: "Read" }],
    });
    const result = scope([prompt, answer, tool], {
      ...filtered(["user", "assistant"]),
      transcriptMode: "focused",
      revealOrdinal: 2,
    });
    expect(result.messages).toEqual([prompt, answer, tool]);
    expect(result.allowsBlock(tool, "tool-input")).toBe(true);
  });

  it.each(["normal", "focused"] as const)(
    "keeps filtered blocks hidden for a linked row in %s mode",
    (transcriptMode) => {
      const prompt = message(0, "prompt", { role: "user" });
      const linked = message(1, "[Thinking]\nprivate thought\n[/Thinking]\nanswer", {
        has_thinking: true,
      });
      const final = message(2, "final answer");
      const result = scope([prompt, linked, final], {
        ...filtered(["user", "assistant"]),
        transcriptMode,
        revealOrdinal: 1,
      });
      expect(result.messages).toEqual([prompt, linked, final]);
      expect(result.revealedOrdinal).toBeUndefined();
      expect(result.allowsBlock(linked, "thinking")).toBe(false);
    },
  );

  it("searches the text of a linked answer its role filter hides", () => {
    const prompt = message(0, "prompt", { role: "user" });
    const answer = message(1, "needle answer");
    const result = scope([prompt, answer], { ...filtered(["user"]), revealOrdinal: 1 });
    expect(result.messages).toEqual([prompt, answer]);
    expect(result.allowsBlock(answer, "text")).toBe(true);
  });

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
      tool_calls: [{ call_index: 0, category: "", tool_name: "Bash" }],
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

  it("keeps empty messages whose role is shown when another block type is hidden", () => {
    const emptyAnswer = message(4, "");
    const emptyPrompt = message(5, "", { role: "user" });
    const kept = scope([emptyAnswer, emptyPrompt], filtered(["user", "assistant", "tool"]));
    expect(kept.items.map((item) => [item.kind, item.ordinals])).toEqual([["empty-group", [4, 5]]]);

    const userOnly = scope([emptyAnswer, emptyPrompt], filtered(["user"]));
    expect(userOnly.items.map((item) => [item.kind, item.ordinals])).toEqual([
      ["empty-group", [5]],
    ]);
    expect(userOnly.messages).toEqual([emptyPrompt]);

    const neither = scope([emptyAnswer, emptyPrompt], filtered(["thinking"]));
    expect(neither.items).toEqual([]);
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

  it("keeps skill expansions out of search whichever filter shows them", () => {
    const skill = message(7, "[Skill: review]\nneedle\n[/Skill]", {
      role: "user",
      is_system: true,
      source_subtype: "skill_invocation",
    });
    const prompt = message(8, "check the diff", { role: "user" });
    for (const input of [[skill], [skill, prompt]]) {
      for (const visible of [ALL, new Set<BlockType>(["system"]), new Set<BlockType>(["user"])]) {
        const result = scope(input, {
          visibleBlocks: visible,
          hasBlockFilters: visible.size < ALL.size,
        });
        expect(result.allowsBlock(skill, "text")).toBe(false);
        expect(result.allowsBlock(skill, "skill")).toBe(false);
      }
    }
    expect(scope([skill, prompt]).allowsBlock(prompt, "text")).toBe(true);
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

it.each(["normal", "focused"] as const)(
  "keeps filtered code-only messages expandable in %s mode",
  (transcriptMode) => {
    const prompt = message(0, "Show the example", { role: "user" });
    const code = message(1, "```ts\nconst needle = 1;\n```");
    const result = scope([prompt, code], { ...filtered(["user", "assistant"]), transcriptMode });
    expect(result.items.flatMap((item) => item.ordinals)).toEqual([0, 1]);
    expect(result.allowsBlock(code, "code")).toBe(false);
  },
);
