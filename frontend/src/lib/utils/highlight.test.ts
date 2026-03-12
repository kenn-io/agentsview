// @vitest-environment jsdom
import { describe, it, expect, beforeEach } from "vitest";
import { applyHighlight, escapeHTML } from "./highlight.js";

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

function currentMarks(el: HTMLElement): string[] {
  return Array.from(
    el.querySelectorAll("mark.search-highlight--current"),
  ).map((m) => m.textContent ?? "");
}

describe("applyHighlight", () => {
  describe("initial application", () => {
    it("wraps a single match in a mark element", () => {
      const el = makeDiv("Hello world");
      applyHighlight(el, { q: "world", current: false, content: "" });
      expect(marks(el)).toEqual(["world"]);
    });

    it("wraps multiple matches in the same text node", () => {
      const el = makeDiv("foo bar foo");
      applyHighlight(el, { q: "foo", current: false, content: "" });
      expect(marks(el)).toEqual(["foo", "foo"]);
    });

    it("is case-insensitive", () => {
      const el = makeDiv("Hello WORLD");
      applyHighlight(el, { q: "world", current: false, content: "" });
      expect(marks(el)).toEqual(["WORLD"]);
    });

    it("does nothing when query is empty", () => {
      const el = makeDiv("Hello world");
      applyHighlight(el, { q: "", current: false, content: "" });
      expect(marks(el)).toEqual([]);
    });

    it("does nothing when query is whitespace only", () => {
      const el = makeDiv("Hello world");
      applyHighlight(el, { q: "   ", current: false, content: "" });
      expect(marks(el)).toEqual([]);
    });

    it("does nothing when there are no matches", () => {
      const el = makeDiv("Hello world");
      applyHighlight(el, { q: "xyz", current: false, content: "" });
      expect(marks(el)).toEqual([]);
    });

    it("adds search-highlight--current class when current=true", () => {
      const el = makeDiv("Hello world");
      applyHighlight(el, { q: "world", current: true, content: "" });
      expect(currentMarks(el)).toEqual(["world"]);
    });

    it("does not add --current class when current=false", () => {
      const el = makeDiv("Hello world");
      applyHighlight(el, { q: "world", current: false, content: "" });
      expect(marks(el)).toEqual(["world"]);
      expect(currentMarks(el)).toEqual([]);
    });

    it("preserves surrounding text nodes", () => {
      const el = makeDiv("before match after");
      applyHighlight(el, { q: "match", current: false, content: "" });
      expect(el.textContent).toBe("before match after");
      expect(marks(el)).toEqual(["match"]);
    });

    it("works across nested elements", () => {
      const el = makeDiv("<p>first match</p><p>second match</p>");
      applyHighlight(el, { q: "match", current: false, content: "" });
      expect(marks(el)).toEqual(["match", "match"]);
    });
  });

  describe("update", () => {
    it("replaces old highlights when query changes", () => {
      const el = makeDiv("Hello world");
      const action = applyHighlight(el, {
        q: "Hello",
        current: false,
        content: "",
      });
      expect(marks(el)).toEqual(["Hello"]);

      action.update({ q: "world", current: false, content: "" });
      expect(marks(el)).toEqual(["world"]);
    });

    it("clears marks when query becomes empty on update", () => {
      const el = makeDiv("Hello world");
      const action = applyHighlight(el, {
        q: "Hello",
        current: false,
        content: "",
      });
      expect(marks(el)).toEqual(["Hello"]);

      action.update({ q: "", current: false, content: "" });
      expect(marks(el)).toEqual([]);
    });

    it("updates current class when current changes", () => {
      const el = makeDiv("Hello world");
      const action = applyHighlight(el, {
        q: "world",
        current: false,
        content: "",
      });
      expect(currentMarks(el)).toEqual([]);

      action.update({ q: "world", current: true, content: "" });
      expect(currentMarks(el)).toEqual(["world"]);
    });

    it("leaves original text intact after clearing", () => {
      const el = makeDiv("Hello world");
      const action = applyHighlight(el, {
        q: "world",
        current: false,
        content: "",
      });
      action.update({ q: "", current: false, content: "" });
      expect(el.textContent).toBe("Hello world");
      expect(el.querySelectorAll("mark").length).toBe(0);
    });

    it("re-highlights correctly after innerHTML reset (streaming simulation)", () => {
      // Simulates the streaming fix: content changes via innerHTML replacement
      // (as {@html escapeHTML(content)} does), then update() re-applies marks.
      const el = makeDiv("partial");
      const action = applyHighlight(el, {
        q: "world",
        current: false,
        content: "partial",
      });
      // No match yet
      expect(marks(el)).toEqual([]);

      // Simulate Svelte updating innerHTML (as {@html} does on content change)
      el.innerHTML = "Hello world";
      // Action update fires with new content
      action.update({ q: "world", current: false, content: "Hello world" });

      expect(marks(el)).toEqual(["world"]);
      expect(el.textContent).toBe("Hello world");
    });
  });
});

describe("escapeHTML", () => {
  it("escapes & characters", () => {
    expect(escapeHTML("a & b")).toBe("a &amp; b");
  });

  it("escapes < and > characters", () => {
    expect(escapeHTML("<script>")).toBe("&lt;script&gt;");
  });

  it("escapes double quotes", () => {
    expect(escapeHTML('say "hi"')).toBe("say &quot;hi&quot;");
  });

  it("escapes single quotes", () => {
    expect(escapeHTML("it's")).toBe("it&#39;s");
  });

  it("leaves plain text unchanged", () => {
    expect(escapeHTML("hello world")).toBe("hello world");
  });

  it("escapes all special chars in one string", () => {
    expect(escapeHTML('<a href="x&y">it\'s</a>')).toBe(
      "&lt;a href=&quot;x&amp;y&quot;&gt;it&#39;s&lt;/a&gt;",
    );
  });

  it("returns empty string for empty input", () => {
    expect(escapeHTML("")).toBe("");
  });
});
