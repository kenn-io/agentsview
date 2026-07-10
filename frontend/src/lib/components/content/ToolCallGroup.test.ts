// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
import type { Message } from "../../api/types.js";
// @ts-ignore
import ToolCallGroup from "./ToolCallGroup.svelte";

function makeToolMessage(ordinal: number): Message {
  return {
    id: ordinal,
    session_id: "s1",
    ordinal,
    role: "assistant",
    content: "",
    timestamp: new Date(ordinal * 1000).toISOString(),
    has_thinking: false,
    thinking_text: "",
    has_tool_use: true,
    content_length: 0,
    model: "",
    token_usage: null,
    context_tokens: 0,
    output_tokens: 0,
    has_context_tokens: false,
    has_output_tokens: false,
    is_system: false,
  };
}

describe("ToolCallGroup read progress", () => {
  let component: ReturnType<typeof mount> | undefined;
  let originalObserver: typeof IntersectionObserver | undefined;

  afterEach(() => {
    if (component) unmount(component);
    component = undefined;
    Object.defineProperty(globalThis, "IntersectionObserver", {
      configurable: true,
      writable: true,
      value: originalObserver,
    });
    document.body.innerHTML = "";
  });

  it("reports only the intersecting tool submessage", async () => {
    const callbacks: IntersectionObserverCallback[] = [];
    originalObserver = globalThis.IntersectionObserver;
    class ObserverMock {
      constructor(callback: IntersectionObserverCallback) {
        callbacks.push(callback);
      }

      observe() {}
      disconnect() {}
      unobserve() {}
      takeRecords() { return []; }
      root = null;
      rootMargin = "0px";
      thresholds = [];
    }
    Object.defineProperty(globalThis, "IntersectionObserver", {
      configurable: true,
      writable: true,
      value: ObserverMock,
    });
    const seen: number[] = [];
    component = mount(ToolCallGroup, {
      target: document.body,
      props: {
        messages: [makeToolMessage(4), makeToolMessage(5)],
        timestamp: new Date().toISOString(),
        onMessageVisible: (ordinal: number) => seen.push(ordinal),
      },
    });
    await tick();

    callbacks[1]!([{ isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver);

    expect(seen).toEqual([5]);
  });

  it("reports tool messages without IntersectionObserver", async () => {
    originalObserver = globalThis.IntersectionObserver;
    Object.defineProperty(globalThis, "IntersectionObserver", {
      configurable: true,
      writable: true,
      value: undefined,
    });
    const seen: number[] = [];
    const target = document.createElement("div");
    target.className = "message-list-scroll";
    document.body.append(target);
    const rectSpy = vi
      .spyOn(HTMLElement.prototype, "getBoundingClientRect")
      .mockReturnValue({
        bottom: 100,
        height: 100,
        left: 0,
        right: 100,
        top: 0,
        width: 100,
        x: 0,
        y: 0,
        toJSON: () => ({}),
      });
    component = mount(ToolCallGroup, {
      target,
      props: {
        messages: [makeToolMessage(4)],
        timestamp: new Date().toISOString(),
        onMessageVisible: (ordinal: number) => seen.push(ordinal),
      },
    });
    await tick();

    expect(seen).toEqual([4]);
    rectSpy.mockRestore();
  });

  it("places the divider immediately before its chronological submessage", () => {
    component = mount(ToolCallGroup, {
      target: document.body,
      props: {
        messages: [makeToolMessage(4), makeToolMessage(5)],
        timestamp: new Date().toISOString(),
        divider: { ordinal: 5, label: "New messages" },
      },
    });

    const target = document.querySelector('[data-message-ordinal="5"]');
    expect(target?.previousElementSibling?.classList.contains("read-progress-divider")).toBe(true);
    expect(target?.previousElementSibling?.textContent).toContain("New messages");
  });

  it("places the divider immediately before its newest-first submessage", () => {
    component = mount(ToolCallGroup, {
      target: document.body,
      props: {
        messages: [makeToolMessage(4), makeToolMessage(5)],
        timestamp: new Date().toISOString(),
        sortNewestFirst: true,
        divider: { ordinal: 5, label: "Earlier messages" },
      },
    });

    const target = document.querySelector('[data-message-ordinal="5"]');
    expect(target?.previousElementSibling?.classList.contains("read-progress-divider")).toBe(true);
    expect(target?.previousElementSibling?.textContent).toContain("Earlier messages");
  });
});
