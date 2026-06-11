// @vitest-environment jsdom
import { describe, it, expect, afterEach } from "vitest";
import { mount, unmount, tick } from "svelte";
import { vi } from "vitest";
// @ts-ignore
import CodeBlock from "./CodeBlock.svelte";

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
    if (component) unmount(component);
    document.body.innerHTML = "";
  });

  it("case 1: marks survive Shiki swap (regression guard)", async () => {
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

  it("case 2: query change after Shiki resolved updates marks correctly", async () => {
    // Mount with query "foo".
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
    await vi.waitFor(
      () => {
        if (!codeEl.innerHTML.includes("<span")) throw new Error("not yet");
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
    await vi.waitFor(
      () => {
        if (!codeEl2.innerHTML.includes("<span")) throw new Error("not yet");
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

  it("case 3: unknown language falls back gracefully and still marks", async () => {
    component = mount(CodeBlock, {
      target: document.body,
      props: {
        language: "definitelynotalang",
        content: "some special token here",
        highlightQuery: "special",
        isCurrentHighlight: false,
      },
    });

    // Wait long enough that Shiki would have responded if it were going to.
    await new Promise((r) => setTimeout(r, 200));
    await tick();

    const codeEl = document.body.querySelector("code")!;
    // No Shiki spans expected for an unknown language.
    expect(styledSpans(codeEl)).toHaveLength(0);
    // Search marks must still be applied.
    expect(marks(document.body)).toContain("special");
  });

  it("case 4: no double-marking after Shiki resolves with query active", async () => {
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
