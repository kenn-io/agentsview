import { describe, it, expect } from "vitest";
import {
  isSystemMessage,
  normalizeMessagePreview,
  previewMessage,
} from "./messages.js";
import type { Message } from "../api/types.js";

function msg(overrides: Partial<Message>): Message {
  return {
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

describe("isSystemMessage", () => {
  it("returns true when is_system flag is set", () => {
    expect(isSystemMessage(msg({ is_system: true }))).toBe(true);
  });

  it("returns true for is_system regardless of role", () => {
    expect(
      isSystemMessage(msg({ is_system: true, role: "assistant" })),
    ).toBe(true);
  });

  it("returns false for normal user message", () => {
    expect(
      isSystemMessage(msg({ role: "user", content: "fix the bug" })),
    ).toBe(false);
  });

  it("returns false for assistant message without is_system", () => {
    expect(
      isSystemMessage(msg({ role: "assistant", content: "sure" })),
    ).toBe(false);
  });

  it.each([
    ["continuation", "This session is being continued from a previous..."],
    ["interrupted", "[Request interrupted by user]"],
    ["task-notification", "<task-notification>done</task-notification>"],
    ["command-message", "<command-message>commit</command-message>"],
    ["command-name", "<command-name>/commit</command-name>"],
    ["local-command", "<local-command-output>ok</local-command-output>"],
    ["stop hook", "Stop hook feedback: blocked"],
  ])("detects prefix-based system message: %s", (_label, content) => {
    expect(isSystemMessage(msg({ content }))).toBe(true);
  });

  it("does not match partial prefix", () => {
    expect(
      isSystemMessage(msg({ content: "This session is great" })),
    ).toBe(false);
  });

  it.each([
    "continuation",
    "resume",
    "interrupted",
    "task_notification",
    "stop_hook",
  ])(
    "keeps promoted subtype %s visible even with is_system=true",
    (subtype) => {
      expect(
        isSystemMessage(
          msg({ is_system: true, source_subtype: subtype }),
        ),
      ).toBe(false);
    },
  );

  it("hides unknown source_subtype when is_system is true", () => {
    expect(
      isSystemMessage(
        msg({ is_system: true, source_subtype: "future_subtype" }),
      ),
    ).toBe(true);
  });
});

describe("normalizeMessagePreview", () => {
  it("returns empty string for null/undefined/empty", () => {
    expect(normalizeMessagePreview(null)).toBe("");
    expect(normalizeMessagePreview(undefined)).toBe("");
    expect(normalizeMessagePreview("")).toBe("");
  });

  it("strips <bash-input> tags and prefixes with !", () => {
    expect(
      normalizeMessagePreview(
        "<bash-input>git pull origin main</bash-input>",
      ),
    ).toBe("!git pull origin main");
  });

  it("unwraps <bash-stdout> and <bash-stderr>", () => {
    expect(
      normalizeMessagePreview(
        "<bash-stdout>hello</bash-stdout>",
      ),
    ).toBe("hello");
    expect(
      normalizeMessagePreview(
        "<bash-stderr>oops</bash-stderr>",
      ),
    ).toBe("oops");
  });

  it("normalizes a sequence of input + stdout", () => {
    expect(
      normalizeMessagePreview(
        "<bash-input>echo hi</bash-input>\n<bash-stdout>hi</bash-stdout>",
      ),
    ).toBe("!echo hi\nhi");
  });

  it("leaves plain prose untouched", () => {
    expect(
      normalizeMessagePreview("just a regular message"),
    ).toBe("just a regular message");
  });

  it("trims whitespace inside the wrapper", () => {
    expect(
      normalizeMessagePreview("<bash-input>  ls -la  </bash-input>"),
    ).toBe("!ls -la");
  });
});

describe("previewMessage", () => {
  it("returns empty text and isShell=false for nullish input", () => {
    expect(previewMessage(null)).toEqual({ text: "", isShell: false });
    expect(previewMessage(undefined)).toEqual({ text: "", isShell: false });
    expect(previewMessage("")).toEqual({ text: "", isShell: false });
  });

  it("flags <bash-input> as shell and prefixes with !", () => {
    expect(
      previewMessage("<bash-input>git pull origin main</bash-input>"),
    ).toEqual({ text: "!git pull origin main", isShell: true });
  });

  it("flags <bash-stdout> and <bash-stderr> as shell", () => {
    expect(previewMessage("<bash-stdout>hi</bash-stdout>")).toEqual({
      text: "hi",
      isShell: true,
    });
    expect(previewMessage("<bash-stderr>oops</bash-stderr>")).toEqual({
      text: "oops",
      isShell: true,
    });
  });

  it("does not flag plain prose as shell", () => {
    expect(
      previewMessage("just a regular question about the codebase"),
    ).toEqual({
      text: "just a regular question about the codebase",
      isShell: false,
    });
  });

  it("does not flag a message that merely starts with ! as shell", () => {
    // The ! prefix is only authoritative when paired with a
    // <bash-input> wrapper — a user message that begins with ! in
    // plain prose must not be styled as shell.
    const r = previewMessage("!important: read the README");
    expect(r.isShell).toBe(false);
    expect(r.text).toBe("!important: read the README");
  });

  it("flags shell when an input/stdout pair appears together", () => {
    expect(
      previewMessage(
        "<bash-input>echo hi</bash-input>\n<bash-stdout>hi</bash-stdout>",
      ),
    ).toEqual({ text: "!echo hi\nhi", isShell: true });
  });

  it("normalizeMessagePreview returns previewMessage(text).text", () => {
    const cases = [
      "<bash-input>ls</bash-input>",
      "<bash-stdout>ok</bash-stdout>",
      "plain text",
      "",
    ];
    for (const c of cases) {
      expect(normalizeMessagePreview(c)).toBe(previewMessage(c).text);
    }
  });
});
