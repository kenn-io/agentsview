import { describe, it, expect } from "vitest";
import { buildDisplayItems } from "./display-items.js";
import type { Message } from "../api/types.js";

function msg(
  overrides: Partial<Message> & { content: string },
): Message {
  return {
    id: 1,
    session_id: "s1",
    ordinal: 0,
    role: "assistant",
    timestamp: "2025-02-17T21:04:00Z",
    has_thinking: false,
    has_tool_use: false,
    content_length: overrides.content.length,
    model: "",
    token_usage: null,
    context_tokens: 0,
    output_tokens: 0,
    ...overrides,
  };
}

function toolMsg(
  ordinal: number,
  tool = "Bash",
  args = "$ ls",
) {
  return msg({
    ordinal,
    content: `[${tool}]\n${args}`,
    has_tool_use: true,
  });
}

function textMsg(
  ordinal: number,
  content: string,
  role: "user" | "assistant" = "assistant",
) {
  return msg({ ordinal, content, role });
}

describe("buildDisplayItems", () => {
  it("returns empty array for empty input", () => {
    expect(buildDisplayItems([])).toEqual([]);
  });

  it("wraps all text messages as individual items", () => {
    const msgs = [
      textMsg(0, "Hello"),
      textMsg(1, "Hi", "user"),
      textMsg(2, "How can I help?"),
    ];
    const items = buildDisplayItems(msgs);
    expect(items).toHaveLength(3);
    expect(items.every((i) => i.kind === "message")).toBe(true);
  });

  it("groups all tool-only messages into one group", () => {
    const msgs = [
      toolMsg(0),
      toolMsg(1, "Read", "file.ts"),
      toolMsg(2, "Edit", "changes"),
    ];
    const items = buildDisplayItems(msgs);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({
      kind: "tool-group",
      ordinals: [0, 1, 2],
    });
    expect(items[0]).toHaveProperty("messages.length", 3);
  });

  it("handles mixed text and tool messages", () => {
    const msgs = [
      textMsg(0, "Let me check"),
      toolMsg(1),
      toolMsg(2, "Read", "file.ts"),
      textMsg(3, "Here are the results"),
      toolMsg(4, "Edit", "changes"),
    ];
    const items = buildDisplayItems(msgs);
    expect(items).toHaveLength(4);
    expect(items[0]).toMatchObject({ kind: "message" });
    expect(items[1]).toMatchObject({
      kind: "tool-group",
      ordinals: [1, 2],
    });
    expect(items[1]).toHaveProperty("messages.length", 2);
    expect(items[2]).toMatchObject({ kind: "message" });
    expect(items[3]).toMatchObject({
      kind: "tool-group",
      ordinals: [4],
    });
    expect(items[3]).toHaveProperty("messages.length", 1);
  });

  it("keeps messages with text + tools as single messages", () => {
    const m = msg({
      ordinal: 0,
      content: "Let me explain the output.\n\n[Bash]\n$ ls",
      has_tool_use: true,
    });
    const items = buildDisplayItems([m]);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ kind: "message" });
  });

  it("user messages are always individual items", () => {
    const msgs = [
      msg({
        ordinal: 0,
        role: "user",
        content: "[Bash]\n$ ls",
        has_tool_use: true,
      }),
    ];
    const items = buildDisplayItems(msgs);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ kind: "message" });
  });

  it("single tool-only message becomes a tool-group", () => {
    const items = buildDisplayItems([toolMsg(5)]);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({
      kind: "tool-group",
      ordinals: [5],
    });
  });

  it("uses first message timestamp for tool group", () => {
    const msgs = [
      msg({
        ordinal: 0,
        content: "[Bash]\n$ ls",
        has_tool_use: true,
        timestamp: "2025-02-17T21:04:00Z",
      }),
      msg({
        ordinal: 1,
        content: "[Read]\nfile.ts",
        has_tool_use: true,
        timestamp: "2025-02-17T21:05:00Z",
      }),
    ];
    const items = buildDisplayItems(msgs);
    expect(items[0]).toMatchObject({
      kind: "tool-group",
      timestamp: "2025-02-17T21:04:00Z",
    });
  });
});

describe("buildDisplayItems with skipToolGrouping", () => {
  it("returns empty array for empty input when skipToolGrouping is true", () => {
    expect(buildDisplayItems([], { skipToolGrouping: true })).toEqual([]);
  });

  it("emits tool-only messages as individual MessageItems when skipToolGrouping is true", () => {
    const msgs = [
      toolMsg(0),
      toolMsg(1, "Read", "file.ts"),
      toolMsg(2, "Edit", "changes"),
    ];
    const items = buildDisplayItems(msgs, { skipToolGrouping: true });
    expect(items).toHaveLength(3);
    expect(items.every((i) => i.kind === "message")).toBe(true);
    expect(items.map((i) => i.ordinals)).toEqual([[0], [1], [2]]);
  });

  it("does not create tool-groups when skipToolGrouping is true", () => {
    const msgs = [
      textMsg(0, "Let me check"),
      toolMsg(1),
      toolMsg(2, "Read", "file.ts"),
      textMsg(3, "Results"),
    ];
    const items = buildDisplayItems(msgs, { skipToolGrouping: true });
    expect(items).toHaveLength(4);
    expect(items.every((i) => i.kind === "message")).toBe(true);
  });

  it("still groups tool messages when skipToolGrouping is false (default)", () => {
    const msgs = [toolMsg(0), toolMsg(1, "Read", "file.ts")];
    const itemsDefault = buildDisplayItems(msgs);
    const itemsExplicit = buildDisplayItems(msgs, { skipToolGrouping: false });
    expect(itemsDefault).toHaveLength(1);
    expect(itemsDefault[0]!.kind).toBe("tool-group");
    expect(itemsExplicit).toHaveLength(1);
    expect(itemsExplicit[0]!.kind).toBe("tool-group");
  });

  it("preserves ordinals for individual tool messages when skipToolGrouping is true", () => {
    const msgs = [toolMsg(10), toolMsg(20)];
    const items = buildDisplayItems(msgs, { skipToolGrouping: true });
    expect(items[0]!.ordinals).toEqual([10]);
    expect(items[1]!.ordinals).toEqual([20]);
  });

  it("handles mixed messages with text+tool content when skipToolGrouping is true", () => {
    const mixedMsg = msg({
      ordinal: 0,
      content: "Let me explain.\n\n[Bash]\n$ ls",
      has_tool_use: true,
    });
    const items = buildDisplayItems([mixedMsg], { skipToolGrouping: true });
    expect(items).toHaveLength(1);
    expect(items[0]!.kind).toBe("message");
  });

  it("each message retains its message reference when skipToolGrouping is true", () => {
    const t1 = toolMsg(0);
    const t2 = toolMsg(1, "Read", "file.ts");
    const items = buildDisplayItems([t1, t2], { skipToolGrouping: true });
    expect(items[0]!.kind).toBe("message");
    expect(items[1]!.kind).toBe("message");
    if (items[0]!.kind === "message" && items[1]!.kind === "message") {
      expect(items[0]!.message).toBe(t1);
      expect(items[1]!.message).toBe(t2);
    }
  });
});
