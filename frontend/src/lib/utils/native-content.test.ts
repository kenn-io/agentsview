// @vitest-environment jsdom
import { describe, expect, it } from "vite-plus/test";
import type { DbMessage as Message } from "../api/generated/index.js";
import { collectSearchBlocks } from "../search/block-text.js";
import {
  hasVisibleSegments,
  isToolOnly,
  messageContentIdentity,
  messageSegments,
} from "./content-parser.js";
import { formatMessageForCopy } from "./copy-message.js";

function nativeMessage(overrides: Record<string, unknown> = {}): Message {
  return {
    id: 990001,
    session_id: "native-display",
    ordinal: 3,
    role: "assistant",
    content: "界𐐀 [Thinking]\n\ntail",
    thinking_text: "reasonone\n\nreasontwo",
    tool_result_text: "unmatched",
    content_layout: {
      version: 1,
      blocks: [
        { kind: "text", start: 0, end: 18, call_index: 0 },
        { kind: "thinking", start: 0, end: 9, call_index: 0 },
        { kind: "tool_call", start: 0, end: 0, call_index: 0 },
        { kind: "tool_result", start: 0, end: 9, call_index: 0 },
        { kind: "thinking", start: 11, end: 20, call_index: 0 },
        { kind: "text", start: 20, end: 24, call_index: 0 },
      ],
    },
    tool_calls: [
      {
        tool_name: "Bash",
        category: "Bash",
        rendering: "[Bash]\n$ echo nativecommand",
        input_json: '{"command":"echo nativecommand","_i":"hiddenvalue"}',
        result_content: "attachedresult",
        result_events: [
          {
            event_index: 0,
            source: "tool_result",
            status: "completed",
            content: "attachedresult",
            content_length: 14,
          },
        ],
      },
    ],
    has_context_tokens: false,
    has_output_tokens: false,
    has_thinking: true,
    has_tool_use: true,
    content_length: 100,
    timestamp: "2026-01-01T00:00:00Z",
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
    ...overrides,
  } as Message;
}

describe("native transcript consumers", () => {
  it("keeps cache identities bounded for long transcripts", () => {
    const content = "large transcript ".repeat(10000) + "end";
    const message = nativeMessage({
      content,
      thinking_text: "",
      tool_result_text: "",
      tool_calls: [],
      content_layout: {
        version: 1,
        blocks: [{ kind: "text", start: 0, end: content.length, call_index: 0 }],
      },
    });
    const identity = messageContentIdentity(message);
    expect(identity.toString().length).toBeLessThan(128);
    expect(messageContentIdentity(message)).toBe(identity);
    expect(messageSegments(message)[0]?.content).toBe(content);
  });

  it("refreshes in-place body, layout, tool and event mutations", () => {
    const message = nativeMessage({
      content: "alpha",
      thinking_text: "think",
      tool_result_text: "alone",
      content_layout: {
        version: 1,
        blocks: [
          { kind: "text", start: 0, end: 5, call_index: 0 },
          { kind: "thinking", start: 0, end: 5, call_index: 0 },
          { kind: "tool_result", start: 0, end: 5, call_index: 0 },
          { kind: "tool_call", start: 0, end: 0, call_index: 0 },
        ],
      },
      tool_calls: [
        {
          tool_name: "Bash",
          category: "Bash",
          rendering: "$ cat",
          input_json: "{}",
          result_content: "first",
          result_events: [
            {
              event_index: 0,
              source: "tool_result",
              status: "completed",
              content: "prior",
              content_length: 5,
            },
          ],
        },
      ],
    });
    const original = collectSearchBlocks(message);
    expect(original.map((block) => block.text)).toEqual([
      "alpha\n",
      "think",
      "alone",
      "$ cat",
      "first",
      "prior",
    ]);
    expect(collectSearchBlocks(message)).toBe(original);
    message.content = "omega";
    message.thinking_text = "plans";
    message.tool_result_text = "other";
    message.tool_calls![0]!.rendering = "$ pwd";
    message.tool_calls![0]!.result_content = "later";
    message.tool_calls![0]!.result_events![0]!.content = "final";
    expect(collectSearchBlocks(message).map((block) => block.text)).toEqual([
      "omega\n",
      "plans",
      "other",
      "$ pwd",
      "later",
      "final",
    ]);
    message.content_layout!.blocks[0]!.end = 3;
    expect(collectSearchBlocks(message)[0]?.text).toBe("ome\n");
    message.ordinal = 9;
    expect(collectSearchBlocks(message)[0]?.key).toBe("9:text:0");
    message.is_compact_boundary = true;
    expect(collectSearchBlocks(message)).toEqual([]);
    expect(original.map((block) => block.text)).toEqual([
      "alpha\n",
      "think",
      "alone",
      "$ cat",
      "first",
      "prior",
    ]);
  });

  it("finds rendered owners in native order with UTF-8 spans and literal markers", () => {
    const blocks = collectSearchBlocks(nativeMessage());
    expect(blocks.map(({ kind, text }) => [kind, text])).toEqual([
      ["text", "界𐐀 [Thinking]\n"],
      ["thinking", "reasonone"],
      ["tool-input", "[Bash]\n$ echo nativecommand"],
      ["tool-output", "attachedresult"],
      ["tool-history", "attachedresult"],
      ["tool-output", "unmatched"],
      ["thinking", "reasontwo"],
      ["text", "tail\n"],
    ]);
    expect(new Set(blocks.map((block) => block.key)).size).toBe(8);
  });

  it("copies each owner once in attachment order", () => {
    expect(formatMessageForCopy(nativeMessage())).toBe(
      "界𐐀 [Thinking]\n\n[Thinking]\nreasonone\n[/Thinking]\n\n" +
        "[Bash]\n$ echo nativecommand\n\nattachedresult\n\nunmatched\n\n" +
        "[Thinking]\nreasontwo\n[/Thinking]\n\ntail",
    );
  });

  it("keeps reasoning and standalone output visible through their own filters", () => {
    const reasoning = nativeMessage({
      content: "",
      tool_calls: [],
      tool_result_text: "",
      has_tool_use: false,
      thinking_text: "[redacted]",
      content_layout: {
        version: 1,
        blocks: [{ kind: "thinking", start: 0, end: 10, call_index: 0 }],
      },
    });
    expect(hasVisibleSegments(reasoning, (kind) => kind === "thinking")).toBe(true);
    expect(hasVisibleSegments(reasoning, (kind) => kind === "assistant")).toBe(false);
    const output = nativeMessage({
      content: "",
      tool_calls: [],
      thinking_text: "",
      has_tool_use: false,
      content_layout: {
        version: 1,
        blocks: [{ kind: "tool_result", start: 0, end: 9, call_index: 0 }],
      },
    });
    expect(hasVisibleSegments(output, (kind) => kind === "tool")).toBe(true);
    expect(isToolOnly(output)).toBe(false);
  });

  it("hides native usage-only rows while retaining empty legacy rows", () => {
    const empty = nativeMessage({
      content: "",
      thinking_text: "",
      tool_result_text: "",
      tool_calls: [],
      content_layout: { version: 1, blocks: [] },
    });
    expect(hasVisibleSegments(empty, () => true)).toBe(false);
    expect(hasVisibleSegments({ ...empty, content_layout: null } as Message, () => true)).toBe(
      true,
    );
  });

  it("invalidates equal-length body rewrites for the same message ID", () => {
    const before = nativeMessage({
      content: "alpha",
      thinking_text: "",
      tool_result_text: "",
      tool_calls: [],
      content_layout: { version: 1, blocks: [{ kind: "text", start: 0, end: 5, call_index: 0 }] },
    });
    expect(collectSearchBlocks(before).map((block) => block.text)).toEqual(["alpha\n"]);
    expect(collectSearchBlocks({ ...before, content: "omega" }).map((block) => block.text)).toEqual(
      ["omega\n"],
    );
  });

  it("does not join a code fence across a native reasoning boundary", () => {
    const split = nativeMessage({
      content: "```ts\nfirst\nsecond\n```",
      thinking_text: "between",
      tool_result_text: "",
      tool_calls: [],
      content_layout: {
        version: 1,
        blocks: [
          { kind: "text", start: 0, end: 11, call_index: 0 },
          { kind: "thinking", start: 0, end: 7, call_index: 0 },
          { kind: "text", start: 12, end: 22, call_index: 0 },
        ],
      },
    });
    expect(collectSearchBlocks(split).map((block) => block.kind)).toEqual([
      "text",
      "thinking",
      "text",
    ]);
  });

  it("does not classify literal native skill markers as envelope blocks", () => {
    const literal = nativeMessage({
      content: "[Skill: demo]\nbody\n[/Skill]",
      thinking_text: "",
      tool_result_text: "",
      tool_calls: [],
      content_layout: { version: 1, blocks: [{ kind: "text", start: 0, end: 27, call_index: 0 }] },
    });
    expect(collectSearchBlocks(literal).map(({ kind, text }) => [kind, text])).toEqual([
      ["text", "[Skill: demo]\nbody\n[/Skill]\n"],
    ]);
    expect(
      collectSearchBlocks(nativeMessage({ is_system: true, source_subtype: "continuation" })),
    ).toEqual([]);
    expect(
      hasVisibleSegments(
        nativeMessage({ is_system: true, source_subtype: "continuation" }),
        (kind) => kind === "system",
      ),
    ).toBe(true);
  });

  it("rejects unsupported versions and offsets inside UTF-8 characters", () => {
    expect(() =>
      collectSearchBlocks(nativeMessage({ content_layout: { version: 2, blocks: [] } })),
    ).toThrow();
    expect(() =>
      collectSearchBlocks(
        nativeMessage({
          content_layout: {
            version: 1,
            blocks: [{ kind: "text", start: 1, end: 18, call_index: 0 }],
          },
        }),
      ),
    ).toThrow();
  });

  it("invalidates same-length reasoning, standalone output and call rendering", () => {
    const reasoning = nativeMessage({
      content: "",
      tool_result_text: "",
      thinking_text: "alpha",
      tool_calls: [],
      content_layout: {
        version: 1,
        blocks: [{ kind: "thinking", start: 0, end: 5, call_index: 0 }],
      },
    });
    expect(collectSearchBlocks(reasoning).map((block) => block.text)).toEqual(["alpha"]);
    expect(
      collectSearchBlocks({ ...reasoning, thinking_text: "omega" }).map((block) => block.text),
    ).toEqual(["omega"]);
    const output = nativeMessage({
      content: "",
      thinking_text: "",
      tool_result_text: "alpha",
      tool_calls: [],
      content_layout: {
        version: 1,
        blocks: [{ kind: "tool_result", start: 0, end: 5, call_index: 0 }],
      },
    });
    expect(collectSearchBlocks(output).map((block) => block.text)).toEqual(["alpha"]);
    expect(
      collectSearchBlocks({ ...output, tool_result_text: "omega" }).map((block) => block.text),
    ).toEqual(["omega"]);
    const invocation = nativeMessage({
      content: "",
      thinking_text: "",
      tool_result_text: "",
      content_layout: {
        version: 1,
        blocks: [{ kind: "tool_call", start: 0, end: 0, call_index: 0 }],
      },
      tool_calls: [{ tool_name: "Bash", category: "Bash", rendering: "$ cat", input_json: "{}" }],
    });
    expect(collectSearchBlocks(invocation).map((block) => block.text)).toEqual(["$ cat"]);
    expect(
      collectSearchBlocks({
        ...invocation,
        tool_calls: [{ ...invocation.tool_calls![0]!, rendering: "$ pwd" }],
      }).map((block) => block.text),
    ).toEqual(["$ pwd"]);
  });

  it("attaches tools by their native index and keeps empty reasoning separate from usage", () => {
    const reordered = nativeMessage({
      content: "",
      thinking_text: "",
      tool_result_text: "",
      content_layout: {
        version: 1,
        blocks: [
          { kind: "tool_call", start: 0, end: 0, call_index: 1 },
          { kind: "tool_call", start: 0, end: 0, call_index: 0 },
        ],
      },
      tool_calls: [
        { tool_name: "Read", category: "Read", rendering: "first", input_json: "{}" },
        { tool_name: "Read", category: "Read", rendering: "second", input_json: "{}" },
      ],
    });
    expect(collectSearchBlocks(reordered).map((block) => block.text)).toEqual(["second", "first"]);
    expect(formatMessageForCopy(reordered)).toBe("second\n\nfirst");
    const emptyThinking = nativeMessage({
      content: "",
      thinking_text: "",
      tool_result_text: "",
      tool_calls: [],
      content_layout: {
        version: 1,
        blocks: [{ kind: "thinking", start: 0, end: 0, call_index: 0 }],
      },
    });
    expect(hasVisibleSegments(emptyThinking, (kind) => kind === "thinking")).toBe(true);
    expect(collectSearchBlocks(emptyThinking)).toEqual([]);
  });

  it("parses a code fence across adjacent text spans and preserves the original for copy", () => {
    const adjacent = nativeMessage({
      content: "```ts\nfirst\nsecond\n```",
      thinking_text: "",
      tool_result_text: "",
      tool_calls: [],
      content_layout: {
        version: 1,
        blocks: [
          { kind: "text", start: 0, end: 11, call_index: 0 },
          { kind: "text", start: 12, end: 22, call_index: 0 },
        ],
      },
    });
    expect(collectSearchBlocks(adjacent).map(({ kind, text }) => [kind, text])).toEqual([
      ["code", "first\nsecond\n"],
    ]);
    expect(formatMessageForCopy(adjacent)).toBe("```ts\nfirst\nsecond\n```");
  });
});
