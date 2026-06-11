import { describe, it, expect } from "vitest";
import { highlightToHtml, HIGHLIGHT_MAX_BYTES, HIGHLIGHT_MAX_LINES } from "./syntax-highlight.js";

describe("highlightToHtml", () => {
  describe("known languages", () => {
    it.each([
      ["typescript", "const x = 1;"],
      ["python", "def foo(): pass"],
      ["go", "func main() {}"],
      ["json", '{"key": "value"}'],
      ["bash", "echo hello"],
      ["sql", "SELECT * FROM users;"],
      ["javascript", "var x = 1;"],
    ])("highlights %s code", async (lang, code) => {
      const result = await highlightToHtml(code, lang);
      expect(result).not.toBeNull();
      expect(result).toContain("<span");
    });

    it("preserves the source tokens in the output", async () => {
      const code = "const greeting = 'hello';";
      const result = await highlightToHtml(code, "typescript");
      expect(result).not.toBeNull();
      expect(result).toContain("greeting");
      expect(result).toContain("hello");
    });
  });

  describe("alias resolution", () => {
    it("resolves ts alias to typescript", async () => {
      const result = await highlightToHtml("const x = 1;", "ts");
      expect(result).not.toBeNull();
      expect(result).toContain("<span");
    });

    it("resolves py alias to python", async () => {
      const result = await highlightToHtml("x = 1", "py");
      expect(result).not.toBeNull();
      expect(result).toContain("<span");
    });

    it("resolves sh alias to bash", async () => {
      const result = await highlightToHtml("echo hi", "sh");
      expect(result).not.toBeNull();
      expect(result).toContain("<span");
    });

    it("resolves shell alias to bash", async () => {
      const result = await highlightToHtml("echo hi", "shell");
      expect(result).not.toBeNull();
      expect(result).toContain("<span");
    });

    it("resolves yml alias to yaml", async () => {
      const result = await highlightToHtml("key: value", "yml");
      expect(result).not.toBeNull();
      expect(result).toContain("<span");
    });

    it("resolves golang alias to go", async () => {
      const result = await highlightToHtml("func main() {}", "golang");
      expect(result).not.toBeNull();
      expect(result).toContain("<span");
    });

    it("resolves js alias to javascript", async () => {
      const result = await highlightToHtml("var x = 1;", "js");
      expect(result).not.toBeNull();
      expect(result).toContain("<span");
    });
  });

  describe("unknown languages", () => {
    it("returns null for an unrecognized language", async () => {
      const result = await highlightToHtml("some content", "unknownlang123");
      expect(result).toBeNull();
    });

    it("returns null for diff (not in the preloaded grammar set)", async () => {
      const result = await highlightToHtml("-old\n+new", "diff");
      expect(result).toBeNull();
    });

    it("returns null for an empty language string", async () => {
      const result = await highlightToHtml("const x = 1;", "");
      expect(result).toBeNull();
    });

    it("returns null for a whitespace-only language string", async () => {
      const result = await highlightToHtml("const x = 1;", "   ");
      expect(result).toBeNull();
    });
  });

  describe("size thresholds", () => {
    it("returns null when code exceeds the byte threshold", async () => {
      const oversized = "x".repeat(HIGHLIGHT_MAX_BYTES + 1);
      const result = await highlightToHtml(oversized, "typescript");
      expect(result).toBeNull();
    });

    it("highlights code at exactly HIGHLIGHT_MAX_BYTES bytes", async () => {
      // Exactly at the threshold must still be highlighted (guard is strict >).
      // Use a JSON string value so tokenization stays fast at 50 kB.
      // JSON.stringify of a string with N 'a' chars produces N+2 bytes (quotes).
      const inner = "a".repeat(HIGHLIGHT_MAX_BYTES - 2); // + 2 chars for ""
      const code = JSON.stringify(inner);
      expect(code.length).toBe(HIGHLIGHT_MAX_BYTES);
      const result = await highlightToHtml(code, "json");
      expect(result).not.toBeNull();
    });

    it("returns null when code is HIGHLIGHT_MAX_BYTES + 1 bytes", async () => {
      const oversized = "x".repeat(HIGHLIGHT_MAX_BYTES + 1);
      expect(oversized.length).toBe(HIGHLIGHT_MAX_BYTES + 1);
      const result = await highlightToHtml(oversized, "typescript");
      expect(result).toBeNull();
    });

    it("highlights code with exactly HIGHLIGHT_MAX_LINES lines", async () => {
      // "x\n".repeat(N) produces N lines each ending with \n; split("\n") yields
      // N+1 elements (the last being ""), so we use N-1 repetitions plus a
      // final line without a newline to get exactly HIGHLIGHT_MAX_LINES lines.
      const code = "x\n".repeat(HIGHLIGHT_MAX_LINES - 1) + "x";
      expect(code.split("\n").length).toBe(HIGHLIGHT_MAX_LINES);
      const result = await highlightToHtml(code, "typescript");
      expect(result).not.toBeNull();
    });

    it("returns null when code has HIGHLIGHT_MAX_LINES + 1 lines", async () => {
      const code = "x\n".repeat(HIGHLIGHT_MAX_LINES) + "x";
      expect(code.split("\n").length).toBe(HIGHLIGHT_MAX_LINES + 1);
      const result = await highlightToHtml(code, "typescript");
      expect(result).toBeNull();
    });

    it("returns null when code exceeds the line threshold (bulk check)", async () => {
      const oversized = "x\n".repeat(HIGHLIGHT_MAX_LINES + 1);
      const result = await highlightToHtml(oversized, "typescript");
      expect(result).toBeNull();
    });
  });

  describe("output safety", () => {
    it("does not pass through a raw <script> tag from the input", async () => {
      const malicious = "<script>alert(1)</script>";
      const result = await highlightToHtml(malicious, "html");
      // Shiki must have escaped the input; no executable script tag.
      expect(result).not.toBeNull();
      // The raw unescaped tag must not appear in the output.
      expect(result).not.toContain("<script>alert(1)</script>");
      // The escaped form must be present so content-drop can't silently pass.
      // Shiki uses &#x3C; (hex entity) for < and may split tokens into separate
      // spans, so assert the escaped bracket and the tag name are both present.
      expect(result).toContain("&#x3C;");
      expect(result).toContain("script");
    });

    it("does not emit raw angle brackets from javascript code input", async () => {
      // Shiki escapes code tokens; < and > from source must be entities.
      const code = "if (a < b && b > c) {}";
      const result = await highlightToHtml(code, "javascript");
      expect(result).not.toBeNull();
      // The raw < and > inside code tokens should be escaped.
      // We check that the literal source string (with raw brackets) is absent.
      expect(result).not.toContain("a < b");
      expect(result).not.toContain("b > c");
    });

    it("output has no <pre> wrapper (structure: inline)", async () => {
      const result = await highlightToHtml("const x = 1;", "typescript");
      expect(result).not.toBeNull();
      expect(result).not.toContain("<pre");
      expect(result).not.toContain("<code");
    });
  });
});
