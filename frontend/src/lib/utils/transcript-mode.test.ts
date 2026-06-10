import { describe, expect, it } from "vite-plus/test";
import type { Message } from "../api/types.js";
import { buildDisplayItems } from "./display-items.js";
import {
  filterDisplayItemsByTranscriptMode,
  shouldAutoSwitchTranscriptModeToNormal,
} from "./transcript-mode.js";

let nextId = 1;

function msg(
  overrides: Partial<Message> & { content: string },
): Message {
  return {
    id: nextId++,
    session_id: "s1",
    ordinal: 0,
    role: "assistant",
    timestamp: "2025-02-17T21:04:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: overrides.content.length,
    model: "",
    token_usage: null,
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
    ...overrides,
  };
}

function userMsg(ordinal: number, content = "user") {
  return msg({ ordinal, role: "user", content });
}

function assistantMsg(
  ordinal: number,
  content = "assistant",
) {
  return msg({ ordinal, role: "assistant", content });
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

function ordinalsOf(messages: Message[]) {
  const items = buildDisplayItems(messages);
  return filterDisplayItemsByTranscriptMode(
    items,
    "focused",
  ).flatMap((item) => item.ordinals);
}

describe("filterDisplayItemsByTranscriptMode", () => {
  it("returns items unchanged in normal mode", () => {
    const items = buildDisplayItems([
      userMsg(0),
      assistantMsg(1),
      toolMsg(2),
    ]);
    expect(
      filterDisplayItemsByTranscriptMode(items, "normal"),
    ).toEqual(items);
  });

  it("keeps the final assistant before the next user", () => {
    expect(
      ordinalsOf([
        userMsg(0),
        assistantMsg(1, "working"),
        toolMsg(2),
        assistantMsg(3, "final"),
        userMsg(4),
      ]),
    ).toEqual([0, 3, 4]);
  });

  it("drops assistant text that is followed only by tool work before the next user", () => {
    expect(
      ordinalsOf([
        userMsg(0),
        assistantMsg(1, "working"),
        toolMsg(2),
        userMsg(3),
      ]),
    ).toEqual([0, 3]);
  });

  it("keeps the final non-tool assistant at session end", () => {
    expect(
      ordinalsOf([
        userMsg(0),
        toolMsg(1),
        assistantMsg(2, "final"),
      ]),
    ).toEqual([0, 2]);
  });

  it("drops terminal tool-only stretches with no final assistant", () => {
    expect(
      ordinalsOf([userMsg(0), toolMsg(1)]),
    ).toEqual([0]);
  });

  it("keeps only the last assistant in consecutive assistant runs", () => {
    expect(
      ordinalsOf([
        userMsg(0),
        assistantMsg(1, "first"),
        assistantMsg(2, "second"),
        userMsg(3),
      ]),
    ).toEqual([0, 2, 3]);
  });

  it("keeps the assistant response that precedes a compact-boundary divider", () => {
    const boundary = msg({
      ordinal: 2,
      role: "user",
      content: "[compact summary]",
      is_compact_boundary: true,
    });
    expect(
      ordinalsOf([
        userMsg(0),
        assistantMsg(1, "answer"),
        boundary,
        userMsg(3),
      ]),
    ).toEqual([0, 1, 2, 3]);
  });

  it("can pick the last assistant that still has visible segments", () => {
    const items = buildDisplayItems([
      userMsg(0),
      assistantMsg(1, "visible"),
      assistantMsg(2, "hidden"),
      userMsg(3),
    ]);

    expect(
      filterDisplayItemsByTranscriptMode(items, "focused", {
        isMessageVisible: (message) => message.ordinal !== 2,
      }).flatMap((item) => item.ordinals),
    ).toEqual([0, 1, 3]);
  });
});

describe("shouldAutoSwitchTranscriptModeToNormal", () => {
  it("returns true when normal mode would reveal the hidden ordinal", () => {
    const focusedItems = [userMsg(0), userMsg(3)].map(
      (message) => ({
        kind: "message" as const,
        message,
        ordinals: [message.ordinal],
      }),
    );
    const normalItems = [
      userMsg(0),
      assistantMsg(1, "visible in normal"),
      userMsg(3),
    ].map((message) => ({
      kind: "message" as const,
      message,
      ordinals: [message.ordinal],
    }));
    expect(
      shouldAutoSwitchTranscriptModeToNormal(
        "focused",
        1,
        focusedItems,
        normalItems,
      ),
    ).toBe(true);
  });

  it("returns false when the ordinal is already visible", () => {
    const items = [userMsg(0), assistantMsg(1, "final")].map(
      (message) => ({
        kind: "message" as const,
        message,
        ordinals: [message.ordinal],
      }),
    );
    expect(
      shouldAutoSwitchTranscriptModeToNormal(
        "focused",
        1,
        items,
        items,
      ),
    ).toBe(false);
  });

  it("returns false outside focused mode", () => {
    const items = [userMsg(0), assistantMsg(1, "working")].map(
      (message) => ({
        kind: "message" as const,
        message,
        ordinals: [message.ordinal],
      }),
    );
    expect(
      shouldAutoSwitchTranscriptModeToNormal(
        "normal",
        1,
        items,
        items,
      ),
    ).toBe(false);
  });

  it("returns false when normal mode would still not show the ordinal", () => {
    const focusedItems = [userMsg(0), userMsg(3)].map(
      (message) => ({
        kind: "message" as const,
        message,
        ordinals: [message.ordinal],
      }),
    );
    const normalItems = [userMsg(0), userMsg(3)].map(
      (message) => ({
        kind: "message" as const,
        message,
        ordinals: [message.ordinal],
      }),
    );
    expect(
      shouldAutoSwitchTranscriptModeToNormal(
        "focused",
        1,
        focusedItems,
        normalItems,
      ),
    ).toBe(false);
  });
});
