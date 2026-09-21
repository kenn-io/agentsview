// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { setLocale } from "../../i18n/index.js";
import { currentRangeForBlock } from "../../search/search-block.svelte.js";
import CodeBlock from "./CodeBlock.svelte";
const state = vi.hoisted(() => ({ query: "", current: -1, count: 0 }));
const copyMock = vi.hoisted(() => vi.fn().mockResolvedValue(true));
vi.mock("../../utils/clipboard.js", () => ({ copyToClipboard: copyMock }));
vi.mock("../../stores/inSessionSearch.svelte.js", () => ({
  inSessionSearch: {
    get isActive() {
      return !!state.query;
    },
    get debouncedQuery() {
      return state.query;
    },
    countForBlock: () => state.count,
    isCurrentBlock: () => state.current >= 0,
    currentOccurrence: () => state.current,
  },
}));
let component: ReturnType<typeof mount> | undefined;
async function render(content: string, language?: string) {
  component = mount(CodeBlock, {
    target: document.body,
    props: { content, language, searchKey: "7:code:0" },
  });
  await tick();
  return document.querySelector<HTMLElement>(".code-content")!;
}
beforeEach(() => {
  state.query = "";
  state.current = -1;
  state.count = 0;
  copyMock.mockClear();
});
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  document.body.replaceChildren();
  setLocale("en");
});
describe("CodeBlock", () => {
  it("localizes copy labels and preserves raw code", async () => {
    setLocale("zh-CN");
    const source = "const value = 1;\n";
    await render(source);
    const button = document.querySelector<HTMLButtonElement>('button[aria-label="复制代码块"]')!;
    expect(button.getAttribute("title")).toBe("复制代码");
    button.click();
    await Promise.resolve();
    await tick();
    expect(copyMock).toHaveBeenCalledWith(source);
    expect(document.querySelector("code")?.textContent).toBe(source);
  });
  it("keeps cross-token ranges after asynchronous syntax coloring", async () => {
    state.query = "const target";
    state.current = 0;
    state.count = 1;
    const node = await render("const target = 42;\n", "ts");
    await vi.waitFor(
      () => {
        expect(node.querySelector("span[style]")).not.toBeNull();
        expect(currentRangeForBlock(node)?.toString()).toBe("const target");
      },
      { timeout: 10000 },
    );
    expect(
      new Set(
        Array.from(node.querySelectorAll("span[style]"), (span) => span.getAttribute("style")),
      ).size,
    ).toBeGreaterThanOrEqual(2);
    expect(node.textContent).toBe("const target = 42;\n");
    expect(node.querySelector("mark")).toBeNull();
  });
  it("rebuilds a different query after remount without nested wrappers", async () => {
    state.query = "first";
    state.current = 0;
    state.count = 1;
    let node = await render("first second");
    expect(currentRangeForBlock(node)?.toString()).toBe("first");
    await unmount(component!);
    component = undefined;
    state.query = "second";
    node = await render("first second");
    expect(currentRangeForBlock(node)?.toString()).toBe("second");
    expect(node.querySelector("mark")).toBeNull();
  });
  it("retains source and search when the language is unsupported", async () => {
    state.query = "needle";
    state.current = 0;
    state.count = 1;
    const node = await render("<needle>&\n", "not-a-language");
    expect(node.textContent).toBe("<needle>&\n");
    expect(currentRangeForBlock(node)?.toString()).toBe("needle");
    expect(node.querySelector("needle")).toBeNull();
  });
  it("does not create search ranges without an active query", async () => {
    const node = await render("needle needle");
    expect(currentRangeForBlock(node)).toBeUndefined();
    expect(node.querySelector("mark")).toBeNull();
  });
});
