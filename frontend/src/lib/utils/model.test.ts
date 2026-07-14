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

  it("matches the resume mixed-model selection boundary", () => {
    expect(
      computeMainModel([
        msg("assistant", "mixed-model-tie-z"),
        msg("assistant", "mixed-model-tie-a"),
      ]),
    ).toBe("mixed-model-tie-a");
  });

  it("matches UTF-16 tie ordering for astral and BMP models", () => {
    expect(
      computeMainModel([
        msg("assistant", "\uE000"),
        msg("assistant", "\u{10000}"),
      ]),
    ).toBe("\u{10000}");
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

  it("ignores synthetic-only histories", () => {
    expect(
      computeMainModel([
        msg("assistant", "<synthetic>"),
        msg("assistant", "<synthetic>"),
      ]),
    ).toBe("");
  });

  it("ignores synthetic models when real models are present", () => {
    expect(
      computeMainModel([
        msg("assistant", "<synthetic>"),
        msg("assistant", "claude-sonnet-4.6"),
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
