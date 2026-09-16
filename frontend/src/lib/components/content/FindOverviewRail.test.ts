// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { DbMessage as Message } from "../../api/generated/index.js";
import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
import { messages } from "../../stores/messages.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import FindOverviewRail from "./FindOverviewRail.svelte";

let component: ReturnType<typeof mount> | undefined;
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  inSessionSearch.close();
  inSessionSearch.clearQuery();
  messages.clear();
  ui.showAllBlocks();
  document.body.innerHTML = "";
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

it("positions visible matches without hidden thinking, including live filter changes", async () => {
  vi.useFakeTimers();
  vi.spyOn(Element.prototype, "clientHeight", "get").mockReturnValue(100);
  vi.spyOn(Element.prototype, "clientWidth", "get").mockReturnValue(12);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  ui.showAllBlocks();
  ui.transcriptMode = "normal";
  ui.toggleBlock("thinking");
  messages.sessionId = "rail-fixture";
  messages.loading = false;
  messages.hasOlder = false;
  messages.historyComplete = true;
  const content = `[Thinking]\n${"x".repeat(10000)}\n[/Thinking]\n\nneedle`;
  messages.messages = [
    {
      has_context_tokens: false,
      has_output_tokens: false,
      id: 180000,
      session_id: "rail-fixture",
      ordinal: 0,
      role: "assistant",
      content,
      content_length: content.length,
      timestamp: "2026-01-01T00:00:00Z",
      has_thinking: true,
      thinking_text: "",
      has_tool_use: false,
      model: "",
      context_tokens: 0,
      output_tokens: 0,
      is_system: false,
    } satisfies Message,
  ];
  inSessionSearch.open();
  inSessionSearch.query = "needle";
  await vi.advanceTimersByTimeAsync(150);
  await tick();
  component = mount(FindOverviewRail, {
    target: document.body,
    props: {
      items: inSessionSearch.scope!.items,
      totalSize: 100,
      newestFirst: false,
      rowOffset: () => 0,
    },
  });
  await tick();
  expect(document.querySelector("rect")?.getAttribute("y")).toBe("0");

  ui.toggleBlock("thinking");
  await tick();
  expect(document.querySelector("rect")?.getAttribute("y")).toBe("96");

  ui.toggleBlock("thinking");
  await tick();
  expect(document.querySelector("rect")?.getAttribute("y")).toBe("0");
});
