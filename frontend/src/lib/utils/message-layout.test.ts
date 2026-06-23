// ABOUTME: Unit tests for the skim-during-search layout guard.
import { describe, it, expect } from "vite-plus/test";
import { resolveMessageLayout } from "./message-layout.js";

describe("resolveMessageLayout", () => {
  it("returns skim when no highlight is active", () => {
    expect(resolveMessageLayout("skim", false)).toBe("skim");
  });

  it("falls back to default when a highlight is active in skim", () => {
    expect(resolveMessageLayout("skim", true)).toBe("default");
  });

  it("leaves non-skim layouts unchanged while searching", () => {
    expect(resolveMessageLayout("stream", true)).toBe("stream");
    expect(resolveMessageLayout("compact", true)).toBe("compact");
    expect(resolveMessageLayout("default", true)).toBe("default");
  });

  it("leaves non-skim layouts unchanged without search", () => {
    expect(resolveMessageLayout("stream", false)).toBe("stream");
  });
});
