import { describe, expect, it } from "vite-plus/test";
import { codexDesktopLink } from "./codex.js";

describe("codexDesktopLink", () => {
  it("removes the storage prefix from local Codex sessions", () => {
    expect(codexDesktopLink("codex", "codex:thread-123")).toBe(
      "codex://threads/thread-123",
    );
  });

  it("encodes the thread ID as a URL path segment", () => {
    expect(codexDesktopLink("codex", "codex:thread/123")).toBe(
      "codex://threads/thread%2F123",
    );
  });

  it("does not create links for remote or non-Codex sessions", () => {
    expect(codexDesktopLink("codex", "laptop~codex:thread-123")).toBeNull();
    expect(codexDesktopLink("claude", "claude:thread-123")).toBeNull();
  });
});
