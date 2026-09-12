// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { Message } from "../../api/types.js";
import { messages } from "../../stores/messages.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
import { currentRangeForBlock } from "../../search/search-block.svelte.js";
import ToolCallGroup from "./ToolCallGroup.svelte";
let component: ReturnType<typeof mount> | undefined;
let id = 950000;
function message(ordinal: number, structured: boolean): Message {
  const content =
    "[Thinking]\nneedle needle\n[/Thinking]" + (structured ? "" : "\n[Bash]\necho done");
  return {
    id: id++,
    session_id: "group-thinking",
    ordinal,
    role: "assistant",
    content,
    content_length: content.length,
    timestamp: "2026-01-01T00:00:00Z",
    has_thinking: true,
    thinking_text: "",
    has_tool_use: true,
    is_system: false,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    tool_calls: structured
      ? [{ tool_name: "Bash", category: "Bash", result_content: "ordinary output" }]
      : undefined,
  };
}
beforeEach(() => {
  vi.useFakeTimers();
  inSessionSearch.close();
  inSessionSearch.clearQuery();
  messages.clear();
  messages.sessionId = "group-thinking";
  messages.historyComplete = true;
  ui.selectedOrdinal = null;
  ui.sortNewestFirst = false;
  ui.visibleBlocks = new Set(["user", "assistant", "thinking", "tool"]);
});
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  inSessionSearch.close();
  inSessionSearch.clearQuery();
  messages.clear();
  ui.showAllBlocks();
  ui.sortNewestFirst = false;
  document.body.replaceChildren();
  vi.useRealTimers();
});
async function mountGroup(structured: boolean, newestFirst: boolean) {
  messages.messages = [message(7, structured), message(9, structured)];
  ui.sortNewestFirst = newestFirst;
  component = mount(ToolCallGroup, {
    target: document.body,
    props: {
      messages: messages.messages,
      timestamp: "2026-01-01T00:00:00Z",
      searchable: true,
      sortNewestFirst: newestFirst,
    },
  });
  await tick();
}
async function render(structured: boolean, newestFirst: boolean) {
  await mountGroup(structured, newestFirst);
  // Thinking stays rendered by its filter; the search only expands the hit.
  expect(document.querySelectorAll(".thinking-header")).toHaveLength(2);
  expect(document.querySelectorAll(".thinking-content")).toHaveLength(0);
  inSessionSearch.open();
  inSessionSearch.query = "needle";
  await tick();
  await vi.advanceTimersByTimeAsync(150);
  await tick();
}
describe("thinking inside tool groups", () => {
  it.each([
    [true, false],
    [false, false],
    [true, true],
    [false, true],
  ])(
    "reveals indexed thinking for structured=%s newestFirst=%s",
    async (structured, newestFirst) => {
      await render(structured!, newestFirst!);
      expect(inSessionSearch.total).toBe(4);
      const key = `${newestFirst ? 9 : 7}:thinking:0`;
      const block = document.querySelector<HTMLElement>('[data-search-current="true"]')!;
      expect(block.dataset.searchBlock).toBe(key);
      expect(currentRangeForBlock(block)?.toString()).toBe("needle");
      expect(document.querySelectorAll(".thinking-header")).toHaveLength(2);
      expect(document.querySelectorAll(".thinking-content")).toHaveLength(1);
      inSessionSearch.close();
      await tick();
      expect(document.querySelectorAll(".thinking-header")).toHaveLength(2);
      expect(document.querySelectorAll(".thinking-content")).toHaveLength(0);
    },
  );
  it("keeps hidden thinking out of the group index and the DOM", async () => {
    ui.visibleBlocks = new Set(["user", "assistant", "tool"]);
    await mountGroup(true, false);
    expect(document.querySelector(".thinking-header")).toBeNull();
    inSessionSearch.open();
    inSessionSearch.query = "needle";
    await tick();
    await vi.advanceTimersByTimeAsync(150);
    await tick();
    expect(inSessionSearch.total).toBe(0);
    expect(inSessionSearch.countForBlock("7:thinking:0")).toBe(0);
    expect(document.querySelector(".thinking-header")).toBeNull();
    expect(document.querySelector("[data-search-current]")).toBeNull();
  });
  it("keeps manual collapse until the next occurrence is selected", async () => {
    await render(true, false);
    const header = document.querySelector<HTMLButtonElement>(
      '[data-message-ordinal="7"] .thinking-header',
    )!;
    header.click();
    await tick();
    expect(document.querySelector(".thinking-content")).toBeNull();
    inSessionSearch.next();
    await tick();
    expect(document.querySelectorAll(".thinking-content")).toHaveLength(1);
    expect(inSessionSearch.currentOccurrence("7:thinking:0")).toBe(1);
  });
});
