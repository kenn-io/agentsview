// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { mount, tick, unmount } from "svelte";
import type { Message } from "../../api/types.js";
// @ts-ignore
import ToolCallGroup from "./ToolCallGroup.svelte";

function makeToolMessage(ordinal: number): Message {
  return {
    id: ordinal + 1,
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
    tool_calls: [{
      tool_name: "bash",
    }],
    is_system: false,
  };
}

describe("ToolCallGroup", () => {
  it("renders the read-progress divider inside grouped tool rows", async () => {
    const component = mount(ToolCallGroup, {
      target: document.body,
      props: {
        messages: [makeToolMessage(1), makeToolMessage(2)],
        timestamp: "2026-07-11T12:00:00Z",
        divider: {
          ordinal: 2,
          label: "New messages",
        },
      },
    });

    await tick();

    const divider = document.querySelector(".read-progress-divider");
    expect(divider?.textContent).toContain("New messages");
    expect(
      document.querySelector('[data-message-ordinal="2"]'),
    ).not.toBeNull();

    unmount(component);
  });
});
