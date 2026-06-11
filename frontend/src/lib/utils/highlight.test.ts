// @vitest-environment jsdom
import { describe, it, expect, beforeEach } from "vitest";
import { applyHighlight, applyMarks, escapeHTML } from "./highlight.js";

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

describe("applyMarks cross-text-node matching", () => {
  it("marks a phrase that spans sibling <span> elements", () => {
    // Shiki splits "const foo" across three token spans.
    const div = document.createElement("div");
    div.innerHTML =
      "<span>const</span><span> </span><span>foo</span>";

    applyMarks(div, "const foo", false);

    const markEls = Array.from(div.querySelectorAll("mark.search-highlight"));
    // The concatenated textContent of all mark fragments must equal the query.
    const combined = markEls.map((m) => m.textContent ?? "").join("");
    expect(combined).toBe("const foo");
    // No --current class since isCurrent is false.
    expect(
      div.querySelectorAll("mark.search-highlight--current"),
    ).toHaveLength(0);
  });

  it("marks a phrase spanning exactly two adjacent text nodes", () => {
    const div = document.createElement("div");
    // Two sibling spans, match straddles the boundary "ello wo"
    div.innerHTML = "<span>hello </span><span>world</span>";

    applyMarks(div, "ello wo", false);

    const markEls = Array.from(div.querySelectorAll("mark.search-highlight"));
    const combined = markEls.map((m) => m.textContent ?? "").join("");
    expect(combined).toBe("ello wo");
  });

  it("handles partial+full+partial overlap across three nodes", () => {
    const div = document.createElement("div");
    // "ab" spans end of node1, full node2, start of node3: "xab" "cd" "aby"
    // query "abcda" crosses all three: node1 tail "ab", node2 "cd", node3 head "a"
    div.innerHTML = "<span>xab</span><span>cda</span><span>by</span>";

    applyMarks(div, "abcda", false);

    const markEls = Array.from(div.querySelectorAll("mark.search-highlight"));
    const combined = markEls.map((m) => m.textContent ?? "").join("");
    expect(combined).toBe("abcda");
  });

  it("propagates --current class to all mark fragments across nodes", () => {
    const div = document.createElement("div");
    div.innerHTML = "<span>const</span><span> </span><span>foo</span>";

    applyMarks(div, "const foo", true);

    const markEls = Array.from(
      div.querySelectorAll("mark.search-highlight--current"),
    );
    const combined = markEls.map((m) => m.textContent ?? "").join("");
    expect(combined).toBe("const foo");
    expect(markEls.length).toBeGreaterThanOrEqual(1);
  });

  it("still marks single-node matches (regression guard)", () => {
    const div = document.createElement("div");
    div.innerHTML = "<span>foo bar foo</span>";

    applyMarks(div, "foo", false);

    const markEls = Array.from(div.querySelectorAll("mark.search-highlight"));
    expect(markEls.map((m) => m.textContent ?? "")).toEqual(["foo", "foo"]);
  });

  it("finds two matches inside a single segment (cursor no-skip guard)", () => {
    // Two occurrences of "foo" within one text node — the outer cursor must not
    // advance past the segment after the first match.
    const div = document.createElement("div");
    div.innerHTML = "<span>foo bar foo</span>";

    applyMarks(div, "foo", false);

    const markEls = Array.from(div.querySelectorAll("mark.search-highlight"));
    expect(markEls.map((m) => m.textContent ?? "")).toEqual(["foo", "foo"]);
  });

  it("handles many small segments with a 1-char query", () => {
    // Build ~200 sibling spans of 1-2 chars each. The letter "a" appears in
    // every even-indexed span ("a", "bc", "a", "bc", ...).
    const div = document.createElement("div");
    const count = 200;
    let html = "";
    for (let i = 0; i < count; i++) {
      html += i % 2 === 0 ? "<span>a</span>" : "<span>bc</span>";
    }
    div.innerHTML = html;

    applyMarks(div, "a", false);

    const markEls = Array.from(div.querySelectorAll("mark.search-highlight"));
    // 100 even-indexed spans each contribute one "a"
    expect(markEls).toHaveLength(100);
    expect(markEls.every((m) => m.textContent === "a")).toBe(true);
  });

  it("marks a match starting exactly at a segment boundary", () => {
    // "cd" begins at offset 2, which is exactly the start of the second segment.
    // The <= boundary in the outer cursor skip must not overshoot.
    const div = document.createElement("div");
    div.innerHTML = "<span>ab</span><span>cd</span>";

    applyMarks(div, "cd", false);

    const markEls = Array.from(div.querySelectorAll("mark.search-highlight"));
    expect(markEls).toHaveLength(1);
    expect(markEls[0]!.textContent).toBe("cd");
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
