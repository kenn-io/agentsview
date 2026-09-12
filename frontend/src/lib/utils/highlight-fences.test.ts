// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { highlightCodeFences } from "./highlight-fences.js";
import {
  currentRangeForBlock,
  searchBlock,
  type SearchBlockState,
} from "../search/search-block.svelte.js";
const cleanups: (() => void)[] = [];
function fixture(html: string) {
  const root = document.createElement("div");
  root.innerHTML = html;
  document.body.append(root);
  return root;
}
function attach(root: HTMLElement, query: string, count = 1, occurrence = 0) {
  let dispose: (() => void) | undefined;
  const update = (state: SearchBlockState) => {
    dispose?.();
    dispose = searchBlock("7:skill:0", () => state)(root) || undefined;
  };
  update({ query, count, current: true, occurrence });
  cleanups.push(() => dispose?.());
  return { update };
}
function color(root: HTMLElement) {
  const action = highlightCodeFences(root, { content: root.textContent ?? "" });
  cleanups.push(action.destroy);
  return action;
}
afterEach(() => {
  for (const cleanup of cleanups.splice(0).reverse()) cleanup();
  document.body.replaceChildren();
});

describe("highlightCodeFences", () => {
  it.each(["ts", "typescript", "javascript"])(
    "colors %s with multiple token colors",
    async (language) => {
      const root = fixture(
        `<pre><code class="language-${language}">const value = 42;\n</code></pre>`,
      );
      color(root);
      await vi.waitFor(() => expect(root.querySelector("span[style]")).not.toBeNull(), {
        timeout: 10000,
      });
      expect(
        new Set(
          Array.from(root.querySelectorAll("span[style]"), (node) => node.getAttribute("style")),
        ).size,
      ).toBeGreaterThanOrEqual(2);
      expect(root.textContent).toBe("const value = 42;\n");
    },
  );
  it("preserves text for copy after syntax coloring", async () => {
    const root = fixture(
      '<pre><code class="language-ts">const greeting = &quot;hello&quot;;\n</code></pre>',
    );
    color(root);
    await vi.waitFor(() => expect(root.querySelector("span")).not.toBeNull(), { timeout: 10000 });
    expect(root.textContent).toBe('const greeting = "hello";\n');
  });
  it.each(["", ' class="language-diff"'])(
    "leaves unlabeled or unsupported fences literal: %s",
    async (attribute) => {
      const root = fixture(`<pre><code${attribute}>-old\n+new\n</code></pre>`);
      const before = root.innerHTML;
      color(root);
      await new Promise((resolve) => setTimeout(resolve, 200));
      expect(root.innerHTML).toBe(before);
    },
  );
  it("cancels a stale async replacement after content changes", async () => {
    const root = fixture('<pre><code class="language-ts">const old = 1;</code></pre>');
    const action = color(root);
    root.innerHTML = "<p>new content</p>";
    action.update({ content: "new content" });
    await new Promise((resolve) => setTimeout(resolve, 500));
    expect(root.innerHTML).toBe("<p>new content</p>");
  });
  it("rebuilds ranges after syntax spans replace text nodes", async () => {
    const root = fixture('<pre><code class="language-ts">const foo = 1;</code></pre>');
    attach(root, "foo");
    color(root);
    await vi.waitFor(
      () => {
        expect(root.querySelector("span[style]")).not.toBeNull();
        expect(currentRangeForBlock(root)?.toString()).toBe("foo");
      },
      { timeout: 10000 },
    );
    expect(root.querySelector("mark")).toBeNull();
  });
  it("preserves the selected match across adjacent text nodes", async () => {
    const root = fixture("<p>find target here</p>");
    (root.querySelector("p")!.firstChild as Text).splitText(7);
    attach(root, "target");
    color(root);
    await vi.waitFor(() => expect(currentRangeForBlock(root)?.toString()).toBe("target"));
    expect(root.textContent).toBe("find target here");
  });
  it("does not make syntax highlighting responsible for search", async () => {
    const root = fixture('<pre><code class="language-ts">const foo = 1;</code></pre>');
    color(root);
    await vi.waitFor(() => expect(root.querySelector("span")).not.toBeNull(), { timeout: 10000 });
    expect(currentRangeForBlock(root)).toBeUndefined();
    expect(root.querySelector("mark")).toBeNull();
  });
  it("matches across syntax token boundaries", async () => {
    const root = fixture('<pre><code class="language-ts">const foo = 1;</code></pre>');
    attach(root, "const foo");
    color(root);
    await vi.waitFor(
      () => {
        expect(root.querySelector("span")).not.toBeNull();
        expect(currentRangeForBlock(root)?.toString()).toBe("const foo");
      },
      { timeout: 10000 },
    );
  });
  it("keeps prose and code in a single occurrence sequence across updates", async () => {
    const root = fixture(
      '<p>find foo here</p><pre><code class="language-ts">const foo = 1;</code></pre>',
    );
    const handle = attach(root, "foo", 2, 1);
    const action = color(root);
    await vi.waitFor(
      () => {
        expect(root.querySelector("span")).not.toBeNull();
        expect(currentRangeForBlock(root)?.toString()).toBe("foo");
        expect(
          root.querySelector("code")!.contains(currentRangeForBlock(root)!.startContainer),
        ).toBe(true);
      },
      { timeout: 10000 },
    );
    action.update({ content: root.textContent ?? "" });
    handle.update({ query: "foo", count: 2, current: true, occurrence: 0 });
    await vi.waitFor(() =>
      expect(root.querySelector("p")!.contains(currentRangeForBlock(root)!.startContainer)).toBe(
        true,
      ),
    );
    expect(root.querySelector("mark")).toBeNull();
  });
  it("cancels pending syntax coloring on destruction", async () => {
    const root = fixture('<pre><code class="language-ts">const value = 1;</code></pre>');
    const before = root.innerHTML;
    const action = color(root);
    action.destroy();
    await new Promise((resolve) => setTimeout(resolve, 500));
    expect(root.innerHTML).toBe(before);
  });
});
