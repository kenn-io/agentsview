// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vite-plus/test";
import { highlightToHtml, HIGHLIGHT_MAX_BYTES, HIGHLIGHT_MAX_LINES } from "./syntax-highlight.js";

function spans(html: string): HTMLSpanElement[] {
  const div = document.createElement("div");
  div.innerHTML = html;
  const all = Array.from(div.querySelectorAll("span"));
  return all.filter((s) => s.style.color !== "");
}

function distinctColors(html: string): Set<string> {
  return new Set(spans(html).map((s) => s.getAttribute("style") ?? ""));
}

describe("highlighting worker", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.resetModules();
  });

  it("posts code to the worker and returns its reply", async () => {
    const worker = {
      onmessage: null as ((event: MessageEvent) => void) | null,
      postMessage: vi.fn(({ id }: { id: number }) => {
        queueMicrotask(() =>
          worker.onmessage!(new MessageEvent("message", { data: { id, html: "worker result" } })),
        );
      }),
    };
    vi.stubGlobal(
      "Worker",
      vi.fn(function () {
        return worker;
      }),
    );
    vi.resetModules();
    const { highlightToHtml } = await import("./syntax-highlight.js");

    await expect(highlightToHtml("const x = 1;", "typescript")).resolves.toBe("worker result");
    expect(worker.postMessage).toHaveBeenCalledWith({
      id: expect.any(Number),
      code: "const x = 1;",
      lang: "typescript",
    });
  });

  it.each(["error", "messageerror"])(
    "highlights pending and later requests inline after %s",
    async (event) => {
      const worker = {
        onerror: null as (() => void) | null,
        onmessageerror: null as (() => void) | null,
        terminate: vi.fn(),
        postMessage: vi.fn(() =>
          queueMicrotask(() => {
            if (event === "error") worker.onerror!();
            else worker.onmessageerror!();
          }),
        ),
      };
      const Worker = vi.fn(function () {
        return worker;
      });
      vi.stubGlobal("Worker", Worker);
      vi.resetModules();
      const { highlightToHtml } = await import("./syntax-highlight.js");

      const pending = await Promise.all([
        highlightToHtml("const x = 1;", "typescript"),
        highlightToHtml("const y = 2;", "typescript"),
      ]);
      const later = await highlightToHtml("const z = 3;", "typescript");
      for (const html of [...pending, later]) {
        expect(html).not.toBeNull();
        expect(spans(html!).length).toBeGreaterThan(0);
      }
      expect(worker.terminate).toHaveBeenCalledOnce();
      expect(Worker).toHaveBeenCalledOnce();
      expect(worker.postMessage).toHaveBeenCalledTimes(2);
    },
  );

  it("matches replies to callers when the worker replies in reverse order", async () => {
    const requests: { id: number; code: string; lang: string }[] = [];
    const worker = {
      onmessage: null as ((event: MessageEvent) => void) | null,
      postMessage: vi.fn((request: { id: number; code: string; lang: string }) => {
        requests.push(request);
      }),
    };
    vi.stubGlobal(
      "Worker",
      vi.fn(function () {
        return worker;
      }),
    );
    vi.resetModules();
    const { highlightToHtml } = await import("./syntax-highlight.js");

    const first = highlightToHtml("first", "typescript");
    const second = highlightToHtml("second", "typescript");
    expect(requests).toHaveLength(2);
    for (const { id, code } of requests.toReversed()) {
      worker.onmessage!(new MessageEvent("message", { data: { id, html: code } }));
    }
    await expect(first).resolves.toBe("first");
    await expect(second).resolves.toBe("second");
  });
});

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
      expect(spans(result!).length).toBeGreaterThanOrEqual(1);
    });

    it("uses the catppuccin-mocha palette (keyword color is stable)", async () => {
      // Determinism guard: theme + engine are pinned by the lockfile (shiki 4.2.0,
      // catppuccin-mocha). `const` is a keyword -> #CBA6F7. If this breaks, the
      // theme or engine changed; update intentionally, don't loosen.
      const result = await highlightToHtml("const x = 1;", "typescript");
      expect(result).not.toBeNull();
      const colors = spans(result!).map((s) => s.getAttribute("style"));
      expect(colors).toContain("color:#CBA6F7");
      expect(distinctColors(result!).size).toBeGreaterThanOrEqual(2);
    });
  });

  describe("alias resolution", () => {
    it.each([
      ["ts", "typescript", "const x = 1;"],
      ["py", "python", "x = 1"],
      ["sh", "bash", "echo hi"],
      ["shell", "bash", "echo hi"],
      ["yml", "yaml", "key: value"],
      ["golang", "go", "func main() {}"],
      ["js", "javascript", "var x = 1;"],
    ])("resolves %s alias to %s (identical output)", async (alias, canonical, code) => {
      const [aliasResult, canonicalResult] = await Promise.all([
        highlightToHtml(code, alias),
        highlightToHtml(code, canonical),
      ]);
      expect(canonicalResult).not.toBeNull();
      expect(spans(canonicalResult!).length).toBeGreaterThanOrEqual(1);
      expect(aliasResult).toBe(canonicalResult);
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

    it("output has no <pre> wrapper (structure: inline)", async () => {
      const result = await highlightToHtml("const x = 1;", "typescript");
      expect(result).not.toBeNull();
      expect(result).not.toContain("<pre");
      expect(result).not.toContain("<code");
    });
  });
});
