// @vitest-environment jsdom
import { describe, expect, it } from "vite-plus/test";
import type { Message, ToolCall } from "../api/types.js";
import { renderMarkdown } from "../utils/markdown.js";
import { blockKey, collectSearchBlocks, resolveToolInputText } from "./block-text.js";
import { domText, findOccurrences } from "./dom-text.js";

let nextId = 50000;
function message(content: string, overrides: Partial<Message> = {}): Message {
  return {
    id: nextId++,
    session_id: "search-fixture",
    ordinal: 7,
    role: "assistant",
    content,
    timestamp: "2026-01-01T00:00:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: content.length,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
    ...overrides,
  };
}

function call(tool_name: string, params: Record<string, unknown>): ToolCall {
  return { tool_name, input_json: JSON.stringify(params) };
}

describe("collectSearchBlocks", () => {
  it.each([
    "**needle** and *needle*",
    "[needle](https://example.test/hidden-target)",
    "| heading |\n| --- |\n| needle |",
    "<bash-input>echo needle</bash-input>",
    "<custom>needle</custom>",
    "needle\nnext line",
  ])("uses rendered Markdown text for %s", (content) => {
    const blocks = collectSearchBlocks(message(content));
    const expected = domText(
      new DOMParser().parseFromString(renderMarkdown(content), "text/html").body,
    );
    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.text).toBe(expected);
    expect(findOccurrences(blocks[0]!.text, "needle")).toEqual(findOccurrences(expected, "needle"));
  });

  it("indexes the displayed image placeholder without indexing its serialized metadata", () => {
    const content =
      '[{"type":"input_text","text":"Before"},{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1},{"type":"text","text":"After"}]';
    const blocks = collectSearchBlocks(
      message("", {
        tool_calls: [
          {
            tool_name: "view_image",
            result_content: content,
            result_events: [
              {
                event_index: 0,
                status: "completed",
                source: "tool_result",
                content,
                content_length: content.length,
              },
            ],
          },
        ],
      }),
    );
    expect(
      blocks.filter((block) => block.kind !== "tool-input").map((block) => block.text),
    ).toEqual([
      "Before\n\n[Image: image/png, 3 bytes]\n\nAfter",
      "Before\n\n[Image: image/png, 3 bytes]\n\nAfter",
    ]);
  });

  it("updates Markdown text and offsets when XML display changes for the same message", () => {
    const source = message("<custom>\n\n**needle**\n\n</custom>");
    const normal = collectSearchBlocks(source);
    const preformatted = collectSearchBlocks(source, {
      renderUnknownXmlBlocksAsPreformatted: true,
    });
    expect(normal[0]!.text).toContain("needle");
    expect(normal[0]!.text).not.toContain("**needle**");
    expect(preformatted[0]!.text).toContain("**needle**");
    expect(collectSearchBlocks(source)).toBe(normal);
    expect(collectSearchBlocks(source, { renderUnknownXmlBlocksAsPreformatted: true })).toBe(
      preformatted,
    );
    expect(preformatted[0]!.key).toBe(normal[0]!.key);
  });

  it("does not index hidden link destinations or Markdown delimiters", () => {
    const [block] = collectSearchBlocks(
      message("[**visible**](https://example.test/hidden-target)"),
    );
    expect(block?.text).toContain("visible");
    expect(block?.text).not.toContain("hidden-target");
    expect(block?.text).not.toContain("**");
  });

  it("keeps code and thinking raw and renders skill Markdown", () => {
    const blocks = collectSearchBlocks(
      message(
        "[Thinking]\nneedle **raw**\n[/Thinking]\n\n```ts\nconst needle = 1;\n```\n\n[Skill: demo]\n**needle**\n[/Skill]",
        { has_thinking: true },
      ),
    );
    expect(blocks.find((block) => block.kind === "thinking")?.text).toBe("needle **raw**");
    expect(blocks.find((block) => block.kind === "code")?.text).toContain("const needle = 1;");
    const skill = blocks.find((block) => block.kind === "skill");
    expect(skill?.text).toContain("needle");
    expect(skill?.text).not.toContain("**");
    expect(new Set(blocks.map((block) => block.key)).size).toBe(blocks.length);
  });

  it("uses structured solo input instead of the parsed tool segment", () => {
    const tool = {
      ...call("Bash", { command: "echo actual" }),
      category: "Bash",
      result_content: "output",
    };
    const blocks = collectSearchBlocks(
      message("[Bash]\necho legacy", { has_tool_use: true, tool_calls: [tool] }),
    );
    expect(
      blocks
        .filter((block) => block.kind.startsWith("tool-"))
        .map((block) => [block.key, block.text]),
    ).toEqual([
      ["7:tool-input:0", "command: echo actual"],
      ["7:tool-output:0", "output"],
    ]);
  });

  it("gives parallel calls and each history event distinct keys", () => {
    const tools: ToolCall[] = [
      {
        ...call("Read", { path: "first.txt" }),
        result_content: "first output",
        result_events: [
          {
            source: "tool",
            status: "running",
            content: "earlier",
            content_length: 7,
            event_index: 10,
          },
          {
            source: "tool",
            status: "completed",
            content: "later",
            content_length: 5,
            event_index: 11,
          },
        ],
      },
      call("Read", { path: "second.txt" }),
    ];
    const blocks = collectSearchBlocks(message("", { has_tool_use: true, tool_calls: tools }));
    expect(blocks.map((block) => block.key)).toEqual([
      "7:tool-input:0",
      "7:tool-output:0",
      "7:tool-history:0.0",
      "7:tool-history:0.1",
      "7:tool-input:1",
    ]);
    expect(blocks.map((block) => block.text)).toContain("earlier");
    expect(blocks.map((block) => block.text)).toContain("later");
  });

  it("indexes legacy tools by their filtered segment index", () => {
    const blocks = collectSearchBlocks(
      message("[Bash]\necho needle\n\n[Read]\nnotes.txt", { has_tool_use: true }),
    );
    expect(blocks.filter((block) => block.kind === "tool-input").map((block) => block.key)).toEqual(
      ["7:tool-input:seg0", "7:tool-input:seg1"],
    );
  });

  it("includes generated Edit diff text", () => {
    const blocks = collectSearchBlocks(
      message("", {
        has_tool_use: true,
        tool_calls: [
          call("Edit", {
            old_string: "before",
            new_string: "after",
          }),
        ],
      }),
    );
    expect(blocks[0]?.text).toBe("@@ -1,1 +1,1 @@\n-before\n+after");
  });

  it.each<Partial<Message>>([
    { is_system: true },
    { is_compact_boundary: true },
    { is_system: true, source_subtype: "resume" },
    { role: "user", content: "[Request interrupted by user]" },
  ])("excludes system and boundary messages: %j", (overrides) => {
    expect(collectSearchBlocks(message("needle", overrides))).toEqual([]);
  });

  it("caches by message identity and refreshes replaced tool output", () => {
    const original = message("", { tool_calls: [{ tool_name: "Read", result_content: "first" }] });
    const blocks = collectSearchBlocks(original);
    expect(collectSearchBlocks(original)).toBe(blocks);
    const replacement = {
      ...original,
      tool_calls: [{ tool_name: "Read", result_content: "other" }],
    };
    expect(collectSearchBlocks(replacement)).not.toBe(blocks);
    expect(collectSearchBlocks(replacement)[0]?.text).toBe("other");
    expect(blocks[0]?.text).toBe("first");
  });

  it("does not traverse a referenced subagent transcript", () => {
    const blocks = collectSearchBlocks(
      message("", {
        tool_calls: [
          { ...call("Task", { prompt: "parent prompt" }), subagent_session_id: "child-session" },
        ],
      }),
    );
    expect(blocks.map((block) => block.text)).toEqual(["parent prompt"]);
  });
});

describe("resolveToolInputText", () => {
  it.each(["Task", "Agent", "spawn_subagent"])("prefers the %s prompt", (name) => {
    expect(resolveToolInputText(call(name, { prompt: "actual prompt" }), "legacy")).toBe(
      "actual prompt",
    );
  });

  it("recognizes the normalized Task category", () => {
    expect(
      resolveToolInputText(
        { ...call("spawn", { prompt: "actual prompt" }), category: "Task" },
        "legacy",
      ),
    ).toBe("actual prompt");
  });

  it("prefers category fallback, then raw tool name, then legacy content", () => {
    expect(
      resolveToolInputText(
        { ...call("exec_command", { cmd: "echo hello" }), category: "Bash" },
        "",
      ),
    ).toBe("command: echo hello");
    expect(resolveToolInputText(call("custom_tool", { value: "hello" }), "")).toBe("value: hello");
    expect(resolveToolInputText(call("Bash", { command: "ignored" }), "legacy")).toBe("legacy");
    expect(resolveToolInputText(undefined, "legacy")).toBe("legacy");
  });

  it.each(["{broken", "null", "false", "[]"])("handles invalid input %s", (input_json) => {
    expect(resolveToolInputText({ tool_name: "Bash", input_json }, "legacy")).toBe("legacy");
  });

  it("retains legacy text when a Task prompt is empty", () => {
    expect(resolveToolInputText(call("Task", { prompt: "" }), "legacy")).toBe("legacy");
  });

  it("constructs keys without treating punctuation as a query", () => {
    expect(blockKey(42, "tool-history", "2.3")).toBe("42:tool-history:2.3");
  });
});
