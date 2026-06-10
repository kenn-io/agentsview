import { describe, it, expect } from "vite-plus/test";
import { computeMainModel } from "./model.js";
import type { Message } from "../api/types.js";

function msg(role: string, model: string): Message {
  return {
    id: 0,
    session_id: "",
    ordinal: 0,
    role,
    content: "",
    timestamp: "",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: 0,
    model,
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
  };
}

describe("computeMainModel", () => {
  it("returns empty string for empty array", () => {
    expect(computeMainModel([])).toBe("");
  });

  it("returns the single model", () => {
    expect(
      computeMainModel([
        msg("assistant", "claude-sonnet-4.6"),
      ]),
    ).toBe("claude-sonnet-4.6");
  });

  it("returns most frequent model", () => {
    expect(
      computeMainModel([
        msg("assistant", "claude-sonnet-4.6"),
        msg("assistant", "claude-sonnet-4.6"),
        msg("assistant", "claude-haiku-4.5"),
      ]),
    ).toBe("claude-sonnet-4.6");
  });

  it("breaks ties alphabetically", () => {
    expect(
      computeMainModel([
        msg("assistant", "b-model"),
        msg("assistant", "a-model"),
      ]),
    ).toBe("a-model");
  });

  it("ignores user messages", () => {
    expect(
      computeMainModel([
        msg("user", "some-model"),
        msg("assistant", "claude-sonnet-4.6"),
      ]),
    ).toBe("claude-sonnet-4.6");
  });

  it("ignores empty model strings", () => {
    expect(
      computeMainModel([
        msg("assistant", ""),
        msg("assistant", "claude-sonnet-4.6"),
      ]),
    ).toBe("claude-sonnet-4.6");
  });

  it("returns empty when no model data", () => {
    expect(
      computeMainModel([
        msg("assistant", ""),
        msg("user", ""),
      ]),
    ).toBe("");
  });
});
