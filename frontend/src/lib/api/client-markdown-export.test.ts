// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vite-plus/test";
import { getMarkdownExportUrl } from "./client.js";

const storage = {
  getItem: vi.fn().mockReturnValue(""),
  setItem: vi.fn(),
  removeItem: vi.fn(),
  clear: vi.fn(),
};

describe("markdown export URLs", () => {
  beforeEach(() => {
    vi.stubGlobal("localStorage", storage);
    storage.getItem.mockReturnValue("");
    document.head.innerHTML = "";
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("builds markdown export URL with optional depth", () => {
    expect(getMarkdownExportUrl("sess-123")).toBe(
      "/api/v1/sessions/sess-123/md",
    );
    expect(getMarkdownExportUrl("sess-123", "all")).toBe(
      "/api/v1/sessions/sess-123/md?depth=all",
    );
    expect(getMarkdownExportUrl("sess-123", 1)).toBe(
      "/api/v1/sessions/sess-123/md?depth=1",
    );
  });
});
