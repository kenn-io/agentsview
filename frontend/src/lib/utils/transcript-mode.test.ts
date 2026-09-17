import { describe, expect, it } from "vite-plus/test";
import type { DbMessage as Message } from "../api/generated/index.js";
import { buildDisplayItems } from "./display-items.js";
import {
  filterDisplayItemsByTranscriptMode,
  shouldAutoSwitchTranscriptModeToNormal,
} from "./transcript-mode.js";

let nextId = 1;

function msg(overrides: Partial<Message> & { content: string }): Message {
  return {
    has_context_tokens: false,
    has_output_tokens: false,
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
  return msg({
    has_context_tokens: false,
    has_output_tokens: false,
    ordinal,
    role: "user",
    content,
  });
}

function assistantMsg(ordinal: number, content = "assistant") {
  return msg({
    has_context_tokens: false,
    has_output_tokens: false,
    ordinal,
    role: "assistant",
    content,
  });
}

function toolMsg(ordinal: number, tool = "Bash", args = "$ ls") {
  return msg({
    ordinal,
    content: `[${tool}]\n${args}`,
    has_tool_use: true,
  });
}

function systemMsg(ordinal: number, subtype: string, content: string) {
  return msg({
    has_context_tokens: false,
    has_output_tokens: false,
    ordinal,
    role: "user",
    is_system: true,
    source_subtype: subtype,
    content,
  });
}

function ordinalsOf(messages: Message[], keepAnswerBeforeTrailingTools = false) {
  const items = buildDisplayItems(messages);
  return filterDisplayItemsByTranscriptMode(items, "focused", {
    keepAnswerBeforeTrailingTools,
  }).flatMap((item) => item.ordinals);
}

describe("filterDisplayItemsByTranscriptMode", () => {
  it("returns items unchanged in normal mode", () => {
    const items = buildDisplayItems([userMsg(0), assistantMsg(1), toolMsg(2)]);
    expect(filterDisplayItemsByTranscriptMode(items, "normal")).toEqual(items);
  });

  it("keeps one turn across task notifications", () => {
    expect(
      ordinalsOf([
        userMsg(0, "review the repo"),
        assistantMsg(1, "spawning four review agents"),
        toolMsg(2, "Task", "review ui"),
        systemMsg(3, "task_notification", "<task-notification>done</task-notification>"),
        assistantMsg(4, "one agent finished, waiting for the rest"),
        toolMsg(5, "Task", "review errors"),
        systemMsg(6, "task_notification", "<task-notification>done</task-notification>"),
        assistantMsg(7, "review complete, report published"),
        userMsg(8, "thanks"),
      ]),
    ).toEqual([0, 7, 8]);
  });

  it("does not let a task notification resurrect dropped narration", () => {
    expect(
      ordinalsOf([
        userMsg(0),
        assistantMsg(1, "working"),
        toolMsg(2),
        systemMsg(3, "task_notification", "<task-notification>done</task-notification>"),
        userMsg(4),
      ]),
    ).toEqual([0, 4]);
  });

  it("keeps one turn across stop hook feedback", () => {
    expect(
      ordinalsOf([
        userMsg(0),
        assistantMsg(1, "first attempt"),
        systemMsg(2, "stop_hook", "Stop hook feedback: tests failed"),
        assistantMsg(3, "fixed and rerun"),
        userMsg(4),
      ]),
    ).toEqual([0, 3, 4]);
  });

  it("still ends the turn at an interruption", () => {
    expect(
      ordinalsOf([
        userMsg(0),
        assistantMsg(1, "partial answer"),
        systemMsg(2, "interrupted", "[Request interrupted by user]"),
        userMsg(3),
        assistantMsg(4, "answer"),
      ]),
    ).toEqual([0, 1, 2, 3, 4]);
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
    expect(ordinalsOf([userMsg(0), assistantMsg(1, "working"), toolMsg(2), userMsg(3)])).toEqual([
      0, 3,
    ]);
  });

  it("keeps an answer when the provider supports post-answer tool work", () => {
    expect(
      ordinalsOf([userMsg(0), assistantMsg(1, "answer"), toolMsg(2), userMsg(3)], true),
    ).toEqual([0, 1, 3]);
  });

  it("still shows nothing when post-answer tool work produced no text", () => {
    expect(ordinalsOf([userMsg(0), toolMsg(1), userMsg(2)], true)).toEqual([0, 2]);
  });

  it("keeps the final non-tool assistant at session end", () => {
    expect(ordinalsOf([userMsg(0), toolMsg(1), assistantMsg(2, "final")])).toEqual([0, 2]);
  });

  it("drops terminal tool-only stretches with no final assistant", () => {
    expect(ordinalsOf([userMsg(0), toolMsg(1)])).toEqual([0]);
  });

  it("keeps only the last assistant in consecutive assistant runs", () => {
    expect(
      ordinalsOf([userMsg(0), assistantMsg(1, "first"), assistantMsg(2, "second"), userMsg(3)]),
    ).toEqual([0, 2, 3]);
  });

  it("keeps the assistant response that precedes a compact-boundary divider", () => {
    const boundary = msg({
      has_context_tokens: false,
      has_output_tokens: false,
      ordinal: 2,
      role: "user",
      content: "[compact summary]",
      is_compact_boundary: true,
    });
    expect(ordinalsOf([userMsg(0), assistantMsg(1, "answer"), boundary, userMsg(3)])).toEqual([
      0, 1, 2, 3,
    ]);
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
    const focusedItems = [userMsg(0), userMsg(3)].map((message) => ({
      kind: "message" as const,
      message,
      ordinals: [message.ordinal],
    }));
    const normalItems = [userMsg(0), assistantMsg(1, "visible in normal"), userMsg(3)].map(
      (message) => ({
        kind: "message" as const,
        message,
        ordinals: [message.ordinal],
      }),
    );
    expect(shouldAutoSwitchTranscriptModeToNormal("focused", 1, focusedItems, normalItems)).toBe(
      true,
    );
  });

  it("returns false when the ordinal is already visible", () => {
    const items = [userMsg(0), assistantMsg(1, "final")].map((message) => ({
      kind: "message" as const,
      message,
      ordinals: [message.ordinal],
    }));
    expect(shouldAutoSwitchTranscriptModeToNormal("focused", 1, items, items)).toBe(false);
  });

  it("returns false outside focused mode", () => {
    const items = [userMsg(0), assistantMsg(1, "working")].map((message) => ({
      kind: "message" as const,
      message,
      ordinals: [message.ordinal],
    }));
    expect(shouldAutoSwitchTranscriptModeToNormal("normal", 1, items, items)).toBe(false);
  });

  it("returns false when normal mode would still not show the ordinal", () => {
    const focusedItems = [userMsg(0), userMsg(3)].map((message) => ({
      kind: "message" as const,
      message,
      ordinals: [message.ordinal],
    }));
    const normalItems = [userMsg(0), userMsg(3)].map((message) => ({
      kind: "message" as const,
      message,
      ordinals: [message.ordinal],
    }));
    expect(shouldAutoSwitchTranscriptModeToNormal("focused", 1, focusedItems, normalItems)).toBe(
      false,
    );
  });
});
