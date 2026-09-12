// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { Message } from "../../api/types.js";
import { messages } from "../../stores/messages.svelte.js";
import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import { setLocale } from "../../i18n/index.js";
import FindResultsList from "./FindResultsList.svelte";
let component: ReturnType<typeof mount> | undefined;
let nextId = 230000;
function message(ordinal: number, content: string): Message {
  return {
    id: nextId++,
    session_id: "results-ui",
    ordinal,
    content,
    role: "assistant",
    content_length: content.length,
    timestamp: "2026-01-01T00:00:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
  };
}
beforeEach(() => {
  vi.useFakeTimers();
  setLocale("en");
  inSessionSearch.close();
  inSessionSearch.clearQuery();
  messages.clear();
  messages.sessionId = "results-ui";
  messages.hasOlder = false;
  messages.loading = false;
  messages.historyComplete = true;
  ui.selectedOrdinal = null;
  ui.sortNewestFirst = false;
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(480);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      private disconnected = false;
      constructor(private callback: ResizeObserverCallback) {}
      observe(target: Element) {
        queueMicrotask(() => {
          if (!this.disconnected)
            this.callback([{ target } as ResizeObserverEntry], this as unknown as ResizeObserver);
        });
      }
      unobserve() {}
      disconnect() {
        this.disconnected = true;
      }
    },
  );
});
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  inSessionSearch.close();
  inSessionSearch.clearQuery();
  messages.clear();
  document.body.replaceChildren();
  setLocale("en");
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});
async function render(source: Message[], query = "needle") {
  messages.messages = source;
  inSessionSearch.open();
  inSessionSearch.query = query;
  await tick();
  await vi.advanceTimersByTimeAsync(150);
  await tick();
  component = mount(FindResultsList, { target: document.body });
  await tick();
  await Promise.resolve();
  await tick();
}
describe("FindResultsList", () => {
  it("shows message groups, match snippets and the subagent exclusion notice", async () => {
    await render([message(1, "needle needle"), message(2, "another needle")]);
    expect(document.querySelectorAll(".result-group")).toHaveLength(2);
    expect(document.querySelectorAll(".find-result-button")).toHaveLength(3);
    expect(
      Array.from(document.querySelectorAll(".result-snippet b"), (node) => node.textContent),
    ).toEqual(["needle", "needle", "needle"]);
    expect(document.querySelector(".result-scope")?.textContent).toContain(
      "Inline subagent messages are excluded.",
    );
  });
  it("selects a specific occurrence through the shared cursor", async () => {
    await render([message(1, "needle needle")]);
    const buttons = document.querySelectorAll<HTMLButtonElement>(".find-result-button");
    buttons[1]!.click();
    await tick();
    expect(inSessionSearch.currentOccurrence("1:text:0")).toBe(1);
    expect(buttons[1]!.classList.contains("kit-button--info")).toBe(true);
  });
  it("activates the keyboard-selected row", async () => {
    await render([message(1, "needle needle")]);
    const list = document.querySelector<HTMLElement>('[role="listbox"]')!;
    expect(list).not.toBeNull();
    list.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true }));
    await tick();
    list.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await tick();
    expect(inSessionSearch.currentOccurrence("1:text:0")).toBe(1);
  });
  it("virtualizes a large number of hits inside one message", async () => {
    await render([message(1, "needle ".repeat(2000))]);
    expect(inSessionSearch.total).toBe(2000);
    const count = document.querySelectorAll(".find-result-button").length;
    expect(count).toBeGreaterThan(0);
    expect(count).toBeLessThan(50);
  });
  it("shows the localized empty state", async () => {
    setLocale("zh-CN");
    await render([message(1, "unrelated")]);
    expect(document.querySelector(".find-results")?.textContent).toContain("当前会话中没有匹配项");
  });
});
