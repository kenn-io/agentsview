// @vitest-environment jsdom
import { describe, expect, it } from "vite-plus/test";
import type { Message } from "../api/types.js";
import { collectSearchBlocks } from "./block-text.js";
import { buildSessionIndex } from "./session-index.js";

let nextId = 100000;
function message(ordinal: number, content: string, overrides: Partial<Message> = {}): Message {
  return {
    id: nextId++,
    session_id: "index-fixture",
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

describe("buildSessionIndex", () => {
  it.each(["", "  ", "\n\t"])("returns an empty index for query %j", (query) => {
    const index = buildSessionIndex([message(0, "needle")], query);
    expect(index.total).toBe(0);
    expect(index.matches).toEqual([]);
    expect(index.byBlock.size).toBe(0);
    expect(index.byOrdinal.size).toBe(0);
  });

  it("counts occurrences rather than messages", () => {
    const index = buildSessionIndex([message(3, "needle needle")], "needle");
    expect(index.matches).toEqual([
      { ordinal: 3, blockKey: "3:text:0", occurrence: 0, start: 0, end: 6 },
      { ordinal: 3, blockKey: "3:text:0", occurrence: 1, start: 7, end: 13 },
    ]);
    expect(index.total).toBe(2);
    expect(index.byBlock.get("3:text:0")).toBe(2);
    expect(index.byOrdinal.get(3)).toBe(2);
  });

  it("is chronological without mutating the input array", () => {
    const source = [message(9, "needle"), message(2, "needle needle"), message(5, "other")];
    const index = buildSessionIndex(source, "needle");
    expect(index.matches.map((match) => match.ordinal)).toEqual([2, 2, 9]);
    expect(source.map((item) => item.ordinal)).toEqual([9, 2, 5]);
    expect([...index.byOrdinal]).toEqual([
      [2, 2],
      [9, 1],
    ]);
  });

  it("uses visible Markdown text rather than source punctuation or hrefs", () => {
    const source = [message(0, "[**needle**](https://example.test/hidden)")];
    expect(buildSessionIndex(source, "needle").total).toBe(1);
    expect(buildSessionIndex(source, "hidden").total).toBe(0);
    expect(buildSessionIndex(source, "**").total).toBe(0);
  });

  it("shares non-overlapping matching semantics", () => {
    const index = buildSessionIndex([message(0, "aaaaa")], "aa");
    expect(index.matches.map(({ start, end }) => [start, end])).toEqual([
      [0, 2],
      [2, 4],
    ]);
  });

  it("retains source UTF-16 offsets for Unicode folds", () => {
    const source = [message(0, "𐐀 İ 中文 𐐨 i 中文")];
    const [block] = collectSearchBlocks(source[0]!);
    const astral = buildSessionIndex(source, "𐐨");
    expect(astral.total).toBe(2);
    expect(astral.matches.map((match) => block!.text.slice(match.start, match.end))).toEqual([
      "𐐀",
      "𐐨",
    ]);
    const latin = buildSessionIndex(source, "i");
    expect(latin.matches.map((match) => block!.text.slice(match.start, match.end))).toEqual([
      "İ",
      "i",
    ]);
    expect(buildSessionIndex(source, "中文").total).toBe(2);
  });

  it("counts tool input, output and history independently", () => {
    const source = [
      message(4, "", {
        has_tool_use: true,
        tool_calls: [
          {
            tool_name: "Bash",
            category: "Bash",
            input_json: JSON.stringify({ command: "needle needle" }),
            result_content: "needle needle needle",
            result_events: [
              {
                source: "tool",
                status: "running",
                content: "needle",
                content_length: 6,
                event_index: 0,
              },
              {
                source: "tool",
                status: "completed",
                content: "needle",
                content_length: 6,
                event_index: 1,
              },
            ],
          },
        ],
      }),
    ];
    const index = buildSessionIndex(source, "needle");
    expect(index.total).toBe(7);
    expect([...index.byBlock]).toEqual([
      ["4:tool-input:0", 2],
      ["4:tool-output:0", 3],
      ["4:tool-history:0.0", 1],
      ["4:tool-history:0.1", 1],
    ]);
    expect(index.byOrdinal.get(4)).toBe(7);
    expect(index.matches.map((match) => match.occurrence)).toEqual([0, 1, 0, 1, 2, 0, 0]);
  });

  it("excludes system and boundary cards", () => {
    const index = buildSessionIndex(
      [
        message(0, "needle", { is_system: true }),
        message(1, "needle", { is_compact_boundary: true }),
        message(2, "needle", { is_system: true, source_subtype: "resume" }),
        message(3, "needle"),
      ],
      "needle",
    );
    expect(index.matches.map((match) => match.ordinal)).toEqual([3]);
  });

  it("keeps old index values immutable when history arrives", () => {
    const current = message(10, "needle");
    const oldIndex = buildSessionIndex([current], "needle");
    const nextIndex = buildSessionIndex(
      [message(1, "needle"), current, message(11, "needle")],
      "needle",
    );
    expect(oldIndex.total).toBe(1);
    expect(oldIndex.matches).toHaveLength(1);
    expect(nextIndex.matches[1]).toEqual(oldIndex.matches[0]);
    expect(nextIndex.total).toBe(3);
  });

  it("skips blocks the caller's scope excludes while keeping the others intact", () => {
    const source = [
      message(4, "", {
        has_tool_use: true,
        tool_calls: [
          {
            tool_name: "Bash",
            category: "Bash",
            input_json: JSON.stringify({ command: "needle" }),
            result_content: "needle needle",
          },
        ],
      }),
      message(5, "needle prose"),
    ];
    const textOnly = buildSessionIndex(
      source,
      "needle",
      (_message, block) => block.kind === "text",
    );
    expect(textOnly.matches.map((match) => match.blockKey)).toEqual(["5:text:0"]);
    expect(textOnly.byBlock.has("4:tool-output:0")).toBe(false);
    const everything = buildSessionIndex(source, "needle", () => true);
    expect(everything.total).toBe(4);
  });

  it("refreshes replaced same-length output data", () => {
    const initial = message(1, "", {
      tool_calls: [{ tool_name: "Read", result_content: "needle" }],
    });
    const replacement = {
      ...initial,
      tool_calls: [{ tool_name: "Read", result_content: "absent" }],
    };
    expect(buildSessionIndex([initial], "needle").total).toBe(1);
    expect(buildSessionIndex([replacement], "needle").total).toBe(0);
  });

  it("indexes older messages that have never been mounted", () => {
    const source = Array.from({ length: 3000 }, (_, ordinal) =>
      message(ordinal, "", {
        tool_calls: [{ tool_name: "Read", result_content: "needle needle" }],
      }),
    );
    const index = buildSessionIndex(source, "needle");
    expect(index.total).toBe(6000);
    expect(index.matches[0]?.ordinal).toBe(0);
    expect(index.matches.at(-1)?.ordinal).toBe(2999);
    expect(index.byOrdinal.size).toBe(3000);
  });
});
