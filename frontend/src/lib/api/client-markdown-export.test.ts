// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vite-plus/test";
import {
  getExportUrl,
  getInsightMarkdownExportUrl,
  getMarkdownExportUrl,
} from "./client.js";

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

  it("keeps the configured remote origin in markdown export URLs", () => {
    storage.getItem.mockImplementation((key: string) =>
      key === "agentsview-server-url"
        ? "https://remote.example.test/agentsview"
        : "",
    );

    expect(getMarkdownExportUrl("sess-123", "all")).toBe(
      "https://remote.example.test/agentsview/api/v1/sessions/sess-123/md?depth=all",
    );
  });

  it("encodes session IDs as one export path segment", () => {
    const sessionId = "deepseek-harness:child%7E/%25?#";

    expect(getExportUrl(sessionId)).toBe(
      "/api/v1/sessions/deepseek-harness%3Achild%257E%2F%2525%3F%23/export",
    );
    expect(getMarkdownExportUrl(sessionId)).toBe(
      "/api/v1/sessions/deepseek-harness%3Achild%257E%2F%2525%3F%23/md",
    );
  });

  it("builds markdown export URL for an insight", () => {
    expect(getInsightMarkdownExportUrl(42)).toBe(
      "/api/v1/insights/42/md",
    );
  });
});
