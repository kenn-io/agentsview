// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { Message, ToolCall } from "../../api/types.js";
import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
import { messages } from "../../stores/messages.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import { collectSearchBlocks } from "../../search/block-text.js";
import { currentRangeForBlock } from "../../search/search-block.svelte.js";
import ThinkingBlock from "./ThinkingBlock.svelte";
import ToolBlock from "./ToolBlock.svelte";
import MessageContent from "./MessageContent.svelte";

const components: ReturnType<typeof mount>[] = [];
let id = 210000;
function message(content: string, overrides: Partial<Message> = {}): Message {
  return {
    id: id++,
    session_id: "blocks",
    ordinal: 7,
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
    ...overrides,
  };
}
async function search(source: Message, query = "needle") {
  messages.messages = [source];
  inSessionSearch.open();
  inSessionSearch.query = query;
  await tick();
  await vi.advanceTimersByTimeAsync(150);
  await tick();
}
beforeEach(() => {
  vi.useFakeTimers();
  inSessionSearch.close();
  inSessionSearch.clearQuery();
  messages.clear();
  messages.sessionId = "blocks";
  messages.loading = false;
  messages.hasOlder = false;
  ui.selectedOrdinal = null;
  ui.sortNewestFirst = false;
  ui.showAllBlocks();
});
afterEach(async () => {
  for (const component of components.splice(0)) await unmount(component);
  inSessionSearch.close();
  inSessionSearch.clearQuery();
  messages.clear();
  document.body.replaceChildren();
  vi.useRealTimers();
});

describe("search block integration", () => {
  it("opens only the current thinking block and respects manual collapse until navigation", async () => {
    const source = message("[Thinking]\nneedle needle\n[/Thinking]", { has_thinking: true });
    await search(source);
    const block = collectSearchBlocks(source).find((item) => item.kind === "thinking")!;
    components.push(
      mount(ThinkingBlock, {
        target: document.body,
        props: { content: block.text, searchKey: block.key },
      }),
    );
    components.push(
      mount(ThinkingBlock, {
        target: document.body,
        props: { content: "needle", searchKey: "8:thinking:0" },
      }),
    );
    await tick();
    expect(document.querySelectorAll(".thinking-content")).toHaveLength(1);
    const header = document.querySelector<HTMLButtonElement>(".thinking-header")!;
    header.click();
    await tick();
    expect(document.querySelectorAll(".thinking-content")).toHaveLength(0);
    await tick();
    expect(header.getAttribute("aria-expanded")).toBe("false");
    inSessionSearch.next();
    await tick();
    expect(document.querySelectorAll(".thinking-content")).toHaveLength(1);
    const content = document.querySelector<HTMLElement>(".thinking-content")!;
    expect(currentRangeForBlock(content)?.toString()).toBe("needle");
    expect(document.querySelectorAll("mark")).toHaveLength(0);
  });

  it("preserves a manual native disclosure choice across incoming messages until navigation", async () => {
    const source = message("<details><summary>Details</summary>needle</details>");
    messages.historyComplete = true;
    await search(source);
    components.push(
      mount(MessageContent, {
        target: document.body,
        props: { message: source, searchOrdinal: 7 },
      }),
    );
    await tick();
    const details = document.querySelector("details")!;
    expect(details.open).toBe(true);
    details.querySelector("summary")!.click();
    await tick();
    expect(details.open).toBe(false);

    messages.messages = [...messages.messages, message("unrelated", { ordinal: 8 })];
    await tick();
    expect(document.querySelector("details")).toBe(details);
    expect(details.open).toBe(false);

    inSessionSearch.next();
    await tick();
    expect(details.open).toBe(true);
    inSessionSearch.close();
    await tick();
    expect(details.open).toBe(false);
  });

  it("reveals truncated input, output and history independently", async () => {
    const command = Array.from({ length: 30 }, (_, index) =>
      index === 29 ? "needle" : `echo ${index}`,
    ).join("\n");
    const call: ToolCall = {
      tool_name: "Bash",
      category: "Bash",
      input_json: JSON.stringify({ command }),
      result_content: "needle output",
      result_events: [
        {
          event_index: 19,
          status: "completed",
          source: "wait_output",
          content: "needle history",
          content_length: 14,
        },
      ],
    };
    await search(message("", { has_tool_use: true, tool_calls: [call] }));
    components.push(
      mount(ToolBlock, {
        target: document.body,
        props: { content: "", toolCall: call, searchScope: { ordinal: 7, callIdx: 0 } },
      }),
    );
    await tick();
    expect(document.querySelector(".tool-content")?.textContent).toContain("needle");
    expect(document.querySelector(".output-content:not(.history-content)")).toBeNull();
    expect(document.querySelector(".history-content")).toBeNull();
    const output = inSessionSearch.matches.find((match) => match.blockKey === "7:tool-output:0")!;
    inSessionSearch.goTo(output);
    await tick();
    expect(document.querySelector(".output-content:not(.history-content)")?.textContent).toBe(
      "needle output",
    );
    expect(document.querySelector(".history-content")).toBeNull();
    document.querySelector<HTMLButtonElement>(".output-header")!.click();
    await tick();
    expect(document.querySelector(".output-content:not(.history-content)")).toBeNull();
    const history = inSessionSearch.matches.find(
      (match) => match.blockKey === "7:tool-history:0.0",
    )!;
    inSessionSearch.goTo(history);
    await tick();
    expect(document.querySelector(".history-content")?.textContent).toBe("needle history");
    expect(document.querySelector(".output-content:not(.history-content)")).toBeNull();
    inSessionSearch.close();
    await tick();
    expect(document.querySelector(".tool-content")).toBeNull();
  });

  it("temporarily displays canonical raw output and restores the formatted preference", async () => {
    const call: ToolCall = { tool_name: "Read", result_content: "# needle\n\n**bold**" };
    const source = message("", { has_tool_use: true, tool_calls: [call] });
    components.push(
      mount(ToolBlock, {
        target: document.body,
        props: { content: "", toolCall: call, searchScope: { ordinal: 7, callIdx: 0 } },
      }),
    );
    await tick();
    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();
    document.querySelector<HTMLButtonElement>(".output-header")!.click();
    await tick();
    document.querySelector<HTMLButtonElement>(".output-mode button:nth-child(2)")!.click();
    await tick();
    expect(document.querySelector(".formatted-output h1")?.textContent).toBe("needle");
    await search(source);
    expect(document.querySelector(".output-content:not(.history-content)")?.textContent).toBe(
      call.result_content,
    );
    expect(document.querySelector(".formatted-output")).toBeNull();
    inSessionSearch.close();
    await tick();
    expect(document.querySelector(".formatted-output h1")?.textContent).toBe("needle");
  });

  it("does not register inline subagent content even when its ordinal collides", async () => {
    const source = message("needle");
    await search(source);
    components.push(
      mount(MessageContent, {
        target: document.body,
        props: { message: source, searchOrdinal: 7, isSubagentContext: true },
      }),
    );
    await tick();
    expect(document.body.textContent).toContain("needle");
    expect(document.querySelector("[data-search-block]")).toBeNull();
  });

  it("keeps literal diff newlines equal to the index", async () => {
    const call: ToolCall = {
      tool_name: "Edit",
      category: "Edit",
      input_json: JSON.stringify({ old_string: "before", new_string: "needle\nnext" }),
    };
    const source = message("", { has_tool_use: true, tool_calls: [call] });
    await search(source);
    components.push(
      mount(ToolBlock, {
        target: document.body,
        props: { content: "", toolCall: call, searchScope: { ordinal: 7, callIdx: 0 } },
      }),
    );
    await tick();
    const expected = collectSearchBlocks(source).find((block) => block.kind === "tool-input")!.text;
    expect(document.querySelector(".diff-view")?.textContent).toBe(expected);
    expect(document.querySelectorAll("mark")).toHaveLength(0);
  });
});
