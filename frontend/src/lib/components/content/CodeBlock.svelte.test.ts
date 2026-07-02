// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from "vitest";
import { mount, unmount, tick } from "svelte";
import CodeBlock from "./CodeBlock.svelte";
import { setLocale } from "../../i18n/index.js";

function marks(el: HTMLElement): string[] {
  return Array.from(el.querySelectorAll("mark.search-highlight")).map(
    (m) => m.textContent ?? "",
  );
}

function styledSpans(el: HTMLElement): HTMLSpanElement[] {
  return Array.from(el.querySelectorAll("span")).filter(
    (s) => (s as HTMLSpanElement).style.color !== "",
  ) as HTMLSpanElement[];
}

describe("CodeBlock syntax highlighting and search marks", () => {
  let component: ReturnType<typeof mount>;

  afterEach(() => {
    setLocale("en");
    if (component) unmount(component);
    document.body.innerHTML = "";
  });

  it("renders copy labels in Simplified Chinese", async () => {
    setLocale("zh-CN");
    component = mount(CodeBlock, {
      target: document.body,
      props: {
        content: "const answer = 42;",
        language: "typescript",
      },
    });
    await tick();

    const copyButton = document.querySelector<HTMLButtonElement>(
      "button.kit-copy-btn",
    );
    expect(copyButton?.getAttribute("aria-label")).toBe("复制代码块");
    expect(copyButton?.getAttribute("title")).toBe("复制代码");
  });

  it("marks survive Shiki swap", async () => {
    component = mount(CodeBlock, {
      target: document.body,
      props: {
        language: "typescript",
        content: "const foo = 1;\nconst bar = foo;",
        highlightQuery: "foo",
        isCurrentHighlight: false,
      },
    });

    const codeEl = document.body.querySelector("code")!;
    expect(codeEl).not.toBeNull();

    // Wait for Shiki to inject syntax-colored spans.
    await vi.waitFor(
      () => {
        if (!codeEl.innerHTML.includes("<span")) throw new Error("not yet");
      },
      { timeout: 10_000 },
    );

    // Give the re-apply effect time to settle after the Shiki swap.
    await tick();
    await tick();

    expect(styledSpans(codeEl).length).toBeGreaterThanOrEqual(1);
    expect(marks(document.body)).toContain("foo");
  });

  it("query change after Shiki resolved updates marks correctly", async () => {
    // Mount with initial query "foo" and wait for Shiki + marks.
    component = mount(CodeBlock, {
      target: document.body,
      props: {
        language: "typescript",
        content: "const foo = 1;\nconst bar = foo;",
        highlightQuery: "foo",
        isCurrentHighlight: false,
      },
    });

    const codeEl = document.body.querySelector("code")!;

    // Wait for Shiki to resolve and marks for "foo" to appear.
    await vi.waitFor(
      () => {
        if (!codeEl.innerHTML.includes("<span")) throw new Error("shiki not yet");
      },
      { timeout: 10_000 },
    );
    await tick();
    await tick();

    expect(marks(document.body)).toContain("foo");

    // Unmount and remount with a different query to simulate a query change.
    unmount(component);
    document.body.innerHTML = "";

    component = mount(CodeBlock, {
      target: document.body,
      props: {
        language: "typescript",
        content: "const foo = 1;\nconst bar = foo;",
        highlightQuery: "bar",
        isCurrentHighlight: false,
      },
    });

    const codeEl2 = document.body.querySelector("code")!;

    // Wait for Shiki to resolve again and marks for "bar" to appear.
    await vi.waitFor(
      () => {
        if (!codeEl2.innerHTML.includes("<span")) throw new Error("shiki not yet");
      },
      { timeout: 10_000 },
    );
    await tick();
    await tick();

    const foundMarks = marks(document.body);
    expect(foundMarks).toContain("bar");
    // Old query must not be marked.
    expect(foundMarks).not.toContain("foo");
  });

  it("unknown language falls back gracefully and still marks", async () => {
    component = mount(CodeBlock, {
      target: document.body,
      props: {
        language: "definitelynotalang",
        content: "some special token here",
        highlightQuery: "special",
        isCurrentHighlight: false,
      },
    });

    // highlightToHtml resolves null quickly for unknown languages; use
    // deterministic microtask flushing instead of a wall-clock wait.
    await vi.waitFor(
      () => {
        // The action must have run; marks are set once tick settles.
        if (marks(document.body).length === 0) throw new Error("not yet");
      },
      { timeout: 5_000 },
    );
    await tick();

    const codeEl = document.body.querySelector("code")!;
    // No Shiki spans expected for an unknown language.
    expect(styledSpans(codeEl)).toHaveLength(0);
    // Search marks must still be applied.
    expect(marks(document.body)).toContain("special");
  });

  it("no double-marking after Shiki resolves with query active", async () => {
    const content = "const foo = 1;\nconst bar = foo;";
    // "foo" appears exactly twice in the content.
    const expectedCount = 2;

    component = mount(CodeBlock, {
      target: document.body,
      props: {
        language: "typescript",
        content,
        highlightQuery: "foo",
        isCurrentHighlight: false,
      },
    });

    const codeEl = document.body.querySelector("code")!;
    await vi.waitFor(
      () => {
        if (!codeEl.innerHTML.includes("<span")) throw new Error("not yet");
      },
      { timeout: 10_000 },
    );
    await tick();
    await tick();

    const markEls = document.body.querySelectorAll("mark.search-highlight");
    expect(markEls).toHaveLength(expectedCount);
  });
});
