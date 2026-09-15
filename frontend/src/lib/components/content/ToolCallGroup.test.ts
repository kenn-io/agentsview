// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { sessionTiming } from "../../stores/sessionTiming.svelte.js";
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
    tool_calls: [
      {
        tool_name: "bash",
      },
    ],
    is_system: false,
  };
}

afterEach(() => {
  sessionTiming.reset();
  document.body.innerHTML = "";
});

describe("ToolCallGroup", () => {
  it("omits duration for legacy calls without stored timing", async () => {
    const message = makeToolMessage(1);
    message.tool_calls = [];
    message.content = "[Bash]\npwd";
    message.content_length = message.content.length;
    const component = mount(ToolCallGroup, {
      target: document.body,
      props: { messages: [message], timestamp: message.timestamp },
    });
    await tick();
    expect(document.querySelector(".tool-duration")).toBeNull();
    unmount(component);
  });

  it.each([
    { duration: 2000, running: false, label: "2.0s" },
    { duration: null, running: false, label: "unknown" },
    { duration: null, running: true, label: "running" },
    { duration: null, running: true, turnDurationMs: 5000, label: "unknown" },
  ])("uses call evidence for $label, running=$running", async ({ duration, running, turnDurationMs, label }) => {
    const message = makeToolMessage(1);
    message.tool_calls = [
      { tool_use_id: "call-1", tool_name: "Bash", input_json: '{"command":"pwd"}' },
    ];
    sessionTiming.timing = {
      session_id: "s1",
      total_duration_ms: 6000,
      tool_duration_ms: duration ?? 0,
      turn_count: 1,
      tool_call_count: 1,
      subagent_count: 0,
      slowest_call: null,
      by_category: [],
      activity: [],
      activity_totals: {
        thinking_ms: 0,
        generation_ms: 0,
        tool_ms: duration ?? 0,
        unattributed_ms: 6000 - (duration ?? 0),
      },
      running,
      turns: [
        {
          message_id: 2,
          ordinal: 1,
          started_at: message.timestamp,
          duration_ms: turnDurationMs ?? (running ? null : 5000),
          primary_category: "Bash",
          calls: [
            {
              tool_use_id: "call-1",
              tool_name: "Bash",
              category: "Bash",
              duration_ms: duration,
              is_parallel: false,
              input_preview: "pwd",
            },
          ],
        },
      ],
    };
    const component = mount(ToolCallGroup, {
      target: document.body,
      props: { messages: [message], timestamp: message.timestamp },
    });
    await tick();

    const actual = document.querySelector(".tool-duration")?.textContent?.trim();
    if (label === "running") expect(actual).toMatch(/^running /);
    else if (label === "unknown") expect(actual).toBeUndefined();
    else expect(actual).toBe(label);
    expect(document.querySelector(".group-label")?.textContent).toContain("1 tool call");
    unmount(component);
  });

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
    expect(document.querySelector('[data-message-ordinal="2"]')).not.toBeNull();

    unmount(component);
  });
});
