import { describe, expect, it } from "vite-plus/test";
import { codexDesktopLink } from "./codex.js";

describe("codexDesktopLink", () => {
  it("removes the storage prefix from local Codex sessions", () => {
    expect(codexDesktopLink("codex", "codex:thread-123")).toBe("codex://threads/thread-123");
  });

  it("encodes the thread ID as a URL path segment", () => {
    expect(codexDesktopLink("codex", "codex:thread/123")).toBe("codex://threads/thread%2F123");
  });

  it("opens the thread of a Codex revert page", () => {
    const thread = "11111111-1111-4111-8111-111111111111";
    expect(
      codexDesktopLink("codex", `codex:${thread}_22222222-2222-4222-8222-222222222222`),
    ).toBe(`codex://threads/${thread}`);
  });

  it("does not create links for remote or non-Codex sessions", () => {
    expect(codexDesktopLink("codex", "laptop~codex:thread-123")).toBeNull();
    expect(codexDesktopLink("claude", "claude:thread-123")).toBeNull();
  });
});
