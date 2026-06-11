// @vitest-environment jsdom
import { describe, it, expect, vi } from "vitest";
import { highlightCodeFences } from "./highlight-fences.js";
import { applyHighlight } from "./highlight.js";

function makeDiv(html: string): HTMLElement {
  const div = document.createElement("div");
  div.innerHTML = html;
  return div;
}

function marks(el: HTMLElement): string[] {
  return Array.from(el.querySelectorAll("mark.search-highlight")).map(
    (m) => m.textContent ?? "",
  );
}

function styledSpans(el: HTMLElement): HTMLSpanElement[] {
  return Array.from(el.querySelectorAll("span")).filter(
    (s) => s.style.color !== "",
  ) as HTMLSpanElement[];
}

function makeMarkdownCodeBlock(lang: string, code: string): string {
  const cls = lang ? ` class="language-${lang}"` : "";
  return `<pre><code${cls}>${code}\n</code></pre>`;
}

describe("highlightCodeFences", () => {
  describe("labeled fences (known language)", () => {
    it("swaps innerHTML of a language-ts code element with <span> tokens", async () => {
      const html = makeMarkdownCodeBlock("ts", "const x = 1;");
      const div = makeDiv(html);
      const action = highlightCodeFences(div, { content: "const x = 1;" });
      const codeEl = div.querySelector("code")!;
      expect(codeEl).not.toBeNull();

      try {
        await vi.waitFor(
          () => {
            if (!codeEl.innerHTML.includes("<span")) throw new Error("not yet");
          },
          { timeout: 10_000 },
        );
        expect(styledSpans(codeEl).length).toBeGreaterThanOrEqual(1);
        const colors = new Set(styledSpans(codeEl).map((s) => s.getAttribute("style")));
        expect(colors.size).toBeGreaterThanOrEqual(2);
      } finally {
        action.destroy();
      }
    });

    it("preserves textContent after the swap (copy still sees full code)", async () => {
      const code = "const greeting = 'hello';";
      const html = makeMarkdownCodeBlock("typescript", code);
      const div = makeDiv(html);
      const action = highlightCodeFences(div, { content: code });
      const codeEl = div.querySelector("code")!;

      try {
        await vi.waitFor(
          () => {
            if (!codeEl.innerHTML.includes("<span")) throw new Error("not yet");
          },
          { timeout: 10_000 },
        );
        // Text content (what copy reads) must contain the original tokens.
        expect(codeEl.textContent).toContain("greeting");
        expect(codeEl.textContent).toContain("hello");
      } finally {
        action.destroy();
      }
    });

    it("highlights a language-javascript fence", async () => {
      const html = makeMarkdownCodeBlock("javascript", "var a = 1;");
      const div = makeDiv(html);
      const action = highlightCodeFences(div, { content: "var a = 1;" });
      const codeEl = div.querySelector("code")!;

      try {
        await vi.waitFor(
          () => {
            if (!codeEl.innerHTML.includes("<span")) throw new Error("not yet");
          },
          { timeout: 10_000 },
        );
        expect(styledSpans(codeEl).length).toBeGreaterThanOrEqual(1);
        const colors = new Set(styledSpans(codeEl).map((s) => s.getAttribute("style")));
        expect(colors.size).toBeGreaterThanOrEqual(2);
      } finally {
        action.destroy();
      }
    });
  });

  describe("unlabeled and unknown fences", () => {
    it("leaves an unlabeled <pre><code> element untouched", async () => {
      const original = "no lang\n";
      const html = `<pre><code>${original}</code></pre>`;
      const div = makeDiv(html);
      const action = highlightCodeFences(div, { content: original });

      try {
        // Give any async work time to settle; nothing should change.
        await new Promise((r) => setTimeout(r, 50));
        const codeEl = div.querySelector("code")!;
        // innerHTML must not have been replaced with <span> tokens.
        expect(codeEl.innerHTML).not.toContain("<span");
        expect(codeEl.textContent).toBe(original);
      } finally {
        action.destroy();
      }
    });

    it("leaves a code element with a diff language tag untouched", async () => {
      const code = "-old line\n+new line\n";
      const html = makeMarkdownCodeBlock("diff", code);
      const div = makeDiv(html);
      const action = highlightCodeFences(div, { content: code });

      try {
        // highlightToHtml returns null for diff (not in preloaded set); wait long
        // enough to confirm the null path does not mutate the DOM.
        await new Promise((r) => setTimeout(r, 200));
        const codeEl = div.querySelector("code")!;
        expect(codeEl.innerHTML).not.toContain("<span");
      } finally {
        action.destroy();
      }
    });
  });

  describe("stale-async guard", () => {
    it("does not apply a highlight from a previous content after content changes", async () => {
      // First render with typescript; immediately update to a different
      // content string before the first highlight resolves.
      const div = makeDiv(makeMarkdownCodeBlock("ts", "const x = 1;"));
      const action = highlightCodeFences(div, { content: "const x = 1;" });

      try {
        // Update with new content (empty, so no fences to highlight).
        div.innerHTML = "<p>plain text, no fences</p>";
        action.update({ content: "plain text, no fences" });

        // Wait well beyond the highlight resolve time to confirm no swap happens.
        await new Promise((r) => setTimeout(r, 500));

        // The first in-flight highlight should have been cancelled; the div
        // now has no code elements so no span swaps should have occurred.
        const codeEl = div.querySelector("code");
        expect(codeEl).toBeNull();
      } finally {
        action.destroy();
      }
    });
  });

  describe("search-highlight interplay", () => {
    it("re-applies search marks inside code after the Shiki swap", async () => {
      const code = "const foo = 1;";
      const html = makeMarkdownCodeBlock("typescript", code);
      const div = makeDiv(html);

      const fenceAction = highlightCodeFences(div, {
        content: code,
        q: "foo",
        current: false,
      });

      const codeEl = div.querySelector("code")!;

      try {
        await vi.waitFor(
          () => {
            if (!codeEl.innerHTML.includes("<span")) throw new Error("not yet");
          },
          { timeout: 10_000 },
        );
        expect(styledSpans(codeEl).length).toBeGreaterThanOrEqual(1);
        const codeMarks = Array.from(
          codeEl.querySelectorAll("mark.search-highlight"),
        ).map((m) => m.textContent ?? "");
        expect(codeMarks).toContain("foo");
      } finally {
        fenceAction.destroy();
      }
    });

    it("does not re-apply marks when no search query is active", async () => {
      const code = "const x = 1;";
      const html = makeMarkdownCodeBlock("ts", code);
      const div = makeDiv(html);
      const action = highlightCodeFences(div, { content: code });
      const codeEl = div.querySelector("code")!;

      try {
        await vi.waitFor(
          () => {
            if (!codeEl.innerHTML.includes("<span")) throw new Error("not yet");
          },
          { timeout: 10_000 },
        );
        // Shiki ran, no marks expected (no query was given).
        expect(codeEl.querySelectorAll("mark.search-highlight")).toHaveLength(0);
      } finally {
        action.destroy();
      }
    });

    it("marks a query that crosses Shiki token boundaries", async () => {
      // "const foo" is split by Shiki into separate <span> tokens;
      // the cross-node applyMarks must still mark the full phrase.
      const code = "const foo = 1;";
      const html = makeMarkdownCodeBlock("typescript", code);
      const div = makeDiv(html);

      const fenceAction = highlightCodeFences(div, {
        content: code,
        q: "const foo",
        current: false,
      });

      const codeEl = div.querySelector("code")!;

      try {
        await vi.waitFor(
          () => {
            if (!codeEl.innerHTML.includes("<span")) throw new Error("not yet");
          },
          { timeout: 10_000 },
        );

        const codeMarks = Array.from(
          codeEl.querySelectorAll("mark.search-highlight"),
        );
        // The mark fragments across token boundaries must concatenate to the query.
        const combined = codeMarks.map((m) => m.textContent ?? "").join("");
        expect(combined).toBe("const foo");
      } finally {
        fenceAction.destroy();
      }
    });

    it("applyHighlight and highlightCodeFences co-applied on the same container", async () => {
      // Mirrors the real MessageContent.svelte call pattern: applyHighlight and
      // highlightCodeFences both mounted on the same <div>.
      const code = "const foo = 1;";
      const prose = "<p>search for foo here</p>";
      const fenceHtml = makeMarkdownCodeBlock("typescript", code);
      const content = prose + fenceHtml;
      const div = makeDiv(content);
      const codeEl = div.querySelector("code")!;

      const hlAction = applyHighlight(div, { q: "foo", current: false, content });
      const fenceAction = highlightCodeFences(div, {
        content,
        q: "foo",
        current: false,
      });

      try {
        // Wait for Shiki to swap code innerHTML.
        await vi.waitFor(
          () => {
            if (!codeEl.innerHTML.includes("<span")) throw new Error("not yet");
          },
          { timeout: 10_000 },
        );

        // Prose <p> must still have marks (Shiki only touched the code element).
        const proseEl = div.querySelector("p")!;
        expect(marks(proseEl)).toContain("foo");

        // Code element must have marks re-applied after the Shiki innerHTML swap.
        expect(marks(codeEl)).toContain("foo");

        // Call update on both actions (simulates Svelte re-rendering the content).
        hlAction.update({ q: "foo", current: false, content });
        fenceAction.update({ content, q: "foo", current: false });

        // After update: applyHighlight re-clears and re-marks all text nodes;
        // highlightCodeFences re-runs the async highlight and re-applies marks.
        // Wait for the Shiki swap to settle again.
        await vi.waitFor(
          () => {
            if (!codeEl.innerHTML.includes("<span")) throw new Error("not yet");
          },
          { timeout: 10_000 },
        );

        // Both prose and code marks must be present after the update cycle.
        expect(marks(proseEl)).toContain("foo");
        expect(marks(codeEl)).toContain("foo");
      } finally {
        hlAction.update({ q: "", current: false, content }); // teardown applyHighlight
        fenceAction.destroy();
      }
    });
  });

  describe("destroy", () => {
    it("cancels in-flight highlights on destroy so stale swaps never occur", async () => {
      const code = "const x = 1;";
      const html = makeMarkdownCodeBlock("ts", code);
      const div = makeDiv(html);
      const originalInner = div.querySelector("code")!.innerHTML;

      const action = highlightCodeFences(div, { content: code });
      // Destroy immediately — should cancel the pending highlight.
      action.destroy();

      // Wait beyond the expected highlight resolution time.
      await new Promise((r) => setTimeout(r, 500));

      const codeEl = div.querySelector("code")!;
      // innerHTML should still be the original plain text, not Shiki spans.
      expect(codeEl.innerHTML).toBe(originalInner);
    });
  });
});
