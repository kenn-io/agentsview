// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { Message } from "../../api/types.js";
import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
import { messages } from "../../stores/messages.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import { setLocale } from "../../i18n/index.js";
import SessionFindBar from "./SessionFindBar.svelte";

const components: ReturnType<typeof mount>[] = [];
beforeEach(() => {
  vi.useFakeTimers();
  inSessionSearch.wholeWord = false;
  ui.selectedOrdinal = null;
  ui.sortNewestFirst = false;
  messages.sessionId = "s1";
  messages.loading = false;
  messages.hasOlder = false;
  messages.historyComplete = true;
});
afterEach(async () => {
  for (const component of components.splice(0)) await unmount(component);
  setLocale("en");
  inSessionSearch.close();
  inSessionSearch.clearQuery();
  messages.clear();
  document.body.innerHTML = "";
  vi.useRealTimers();
});

async function render(query: string) {
  inSessionSearch.open();
  inSessionSearch.query = query;
  components.push(mount(SessionFindBar, { target: document.body }));
  await tick();
  await vi.advanceTimersByTimeAsync(150);
  await tick();
}

describe("SessionFindBar", () => {
  it("renders search controls in Simplified Chinese", async () => {
    setLocale("zh-CN");
    await render("missing");
    expect(document.querySelector('[role="search"]')?.getAttribute("aria-label")).toBe(
      "在会话中查找",
    );
    const input = document.querySelector<HTMLInputElement>(".kit-find-bar__input");
    expect(input?.getAttribute("placeholder")).toBe("在会话中查找...");
    expect(input?.getAttribute("aria-label")).toBe("搜索关键词");
    expect(document.querySelector(".kit-find-bar__counter")?.textContent?.trim()).toBe("无结果");
    expect(document.querySelector(".kit-find-bar__nav-btn")?.getAttribute("aria-label")).toBe(
      "上一个匹配项",
    );
    expect(document.querySelector(".kit-find-bar__close")?.getAttribute("aria-label")).toBe(
      "关闭查找栏",
    );
  });

  it("refocuses and selects the retained query on repeated open", async () => {
    await render("needle");
    const input = document.querySelector<HTMLInputElement>(".kit-find-bar__input")!;
    const other = document.createElement("button");
    document.body.append(other);
    other.focus();
    expect(document.activeElement).toBe(other);
    inSessionSearch.open();
    await tick();
    expect(document.activeElement).toBe(input);
    expect(input.selectionStart).toBe(0);
    expect(input.selectionEnd).toBe(6);
    inSessionSearch.close();
    await tick();
    expect(inSessionSearch.query).toBe("needle");
    expect(document.querySelector(".kit-find-bar")).toBeNull();
  });

  it("announces occurrence counts and advances within one message", async () => {
    const content = "needles needle";
    messages.messages = [
      {
        id: 170000,
        session_id: "s1",
        ordinal: 0,
        role: "assistant",
        content,
        content_length: content.length,
        timestamp: "2026-01-01T00:00:00Z",
        has_thinking: false,
        thinking_text: "",
        has_tool_use: false,
        model: "",
        context_tokens: 0,
        output_tokens: 0,
        is_system: false,
      } satisfies Message,
    ];
    await render("needle");
    expect(document.querySelector(".search-announcement")?.textContent?.trim()).toBe(
      "Match 1 of 2",
    );
    const buttons = document.querySelectorAll<HTMLButtonElement>(".kit-find-bar__nav-btn");
    buttons[1]!.click();
    await tick();
    expect(inSessionSearch.currentOccurrence("0:text:0")).toBe(1);
    expect(document.querySelector(".search-announcement")?.textContent?.trim()).toBe(
      "Match 2 of 2",
    );
    await Promise.resolve();
    const wholeWord = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Match whole word"]',
    )!;
    expect(wholeWord.getAttribute("aria-pressed")).toBe("false");
    wholeWord.click();
    await tick();
    expect(wholeWord.getAttribute("aria-pressed")).toBe("true");
    expect(document.querySelector(".search-announcement")?.textContent?.trim()).toBe(
      "Match 1 of 1",
    );
    wholeWord.click();
    await tick();
    expect(document.querySelector(".search-announcement")?.textContent?.trim()).toBe(
      "Match 1 of 2",
    );
    expect(document.querySelectorAll('[aria-live="polite"]')).toHaveLength(1);
    expect(document.querySelector(".kit-find-bar__counter")?.getAttribute("aria-live")).toBe("off");
  });

  it("toggles the result panel with matching accessible state", async () => {
    setLocale("zh-CN");
    await render("needle");
    const toggle = document.querySelector<HTMLButtonElement>(
      'button[aria-controls="session-find-results"]',
    )!;
    expect(toggle.getAttribute("aria-label")).toBe("显示搜索结果");
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    toggle.click();
    await tick();
    expect(inSessionSearch.resultsOpen).toBe(true);
    expect(toggle.getAttribute("aria-label")).toBe("隐藏搜索结果");
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    toggle.click();
    await tick();
    expect(inSessionSearch.resultsOpen).toBe(false);
  });

  it("closes through the close control and retains the query", async () => {
    await render("needle");
    document.querySelector<HTMLButtonElement>(".kit-find-bar__close")!.click();
    await tick();
    expect(inSessionSearch.isOpen).toBe(false);
    expect(inSessionSearch.query).toBe("needle");
    expect(document.querySelector(".session-find")).toBeNull();
  });
});
