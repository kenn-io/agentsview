// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import type { Message } from "../../api/types.js";
import type { SessionTiming } from "../../api/types/timing.js";
import { m } from "../../i18n/index.js";
// @ts-ignore
import TimingOverview from "./TimingOverview.svelte";

function message(overrides: Partial<Message>): Message {
  return {
    id: 1,
    session_id: "session-1",
    ordinal: 0,
    role: "user",
    content: "Investigate the failing build",
    timestamp: "2026-08-30T10:00:00.000Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: 29,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
    ...overrides,
  };
}

function fixture(): { messages: Message[]; timing: SessionTiming } {
  const messages = [
    message({ ordinal: 0 }),
    message({
      id: 2,
      ordinal: 1,
      role: "assistant",
      content: "Running the focused test.",
      timestamp: "2026-08-30T10:00:02.000Z",
      has_tool_use: true,
      model: "deepseek-v3.2",
      tool_calls: [
        {
          tool_use_id: "call-1",
          tool_name: "exec_command",
          category: "Bash",
          result_events: [
            {
              tool_use_id: "call-1",
              source: "tool_execution",
              status: "started",
              content: "",
              content_length: 0,
              timestamp: "2026-08-30T10:00:02.250Z",
              event_index: 0,
            },
            {
              tool_use_id: "call-1",
              source: "tool_execution",
              status: "completed",
              content: "ok",
              content_length: 2,
              timestamp: "2026-08-30T10:00:05.750Z",
              event_index: 1,
            },
          ],
        },
      ],
    }),
    message({
      id: 3,
      ordinal: 2,
      timestamp: "2026-08-30T10:00:06.000Z",
      content: "Tool result",
    }),
  ];
  const timing: SessionTiming = {
    session_id: "session-1",
    total_duration_ms: 10_000,
    tool_duration_ms: 4_000,
    turn_count: 1,
    tool_call_count: 1,
    subagent_count: 0,
    slowest_call: null,
    by_category: [{ category: "Bash", duration_ms: 4_000, call_count: 1 }],
    turns: [
      {
        message_id: 2,
        ordinal: 1,
        started_at: "2026-08-30T10:00:02.000Z",
        duration_ms: 4_000,
        primary_category: "Bash",
        calls: [
          {
            tool_use_id: "call-1",
            tool_name: "exec_command",
            category: "Bash",
            duration_ms: 4_000,
            is_parallel: false,
            input_preview: "go test ./...",
          },
        ],
      },
    ],
    running: false,
  };
  return { messages, timing };
}

afterEach(() => cleanup());

describe("TimingOverview", () => {
  it("renders the three recorded-time lanes and navigates from a span", async () => {
    const onNavigate = vi.fn();
    const data = fixture();
    render(TimingOverview, {
      ...data,
      sessionStartedAt: "2026-08-30T10:00:00.000Z",
      sessionEndedAt: "2026-08-30T10:00:10.000Z",
      onNavigate,
    });

    expect(screen.getByText(m.session_vitals_timeline_input())).toBeTruthy();
    expect(screen.getByText(m.session_vitals_timeline_model())).toBeTruthy();
    expect(screen.getByText(m.session_vitals_timeline_tools())).toBeTruthy();

    const tool = document.querySelector<HTMLButtonElement>('[data-overview-span^="tool:"]');
    expect(tool).not.toBeNull();
    expect(tool?.title).toContain("Bash");
    expect(tool?.title).toContain("3.5s");

    await fireEvent.click(tool!);
    expect(onNavigate).toHaveBeenCalledWith(1);
  });

  it("focuses the selected interval and exposes a clear action", async () => {
    const onNavigate = vi.fn();
    const data = fixture();
    render(TimingOverview, { ...data, onNavigate });
    const track = document.querySelector<HTMLElement>(".overview-track")!;
    track.getBoundingClientRect = () => ({
      x: 0,
      y: 0,
      top: 0,
      right: 200,
      bottom: 64,
      left: 0,
      width: 200,
      height: 64,
      toJSON: () => ({}),
    });

    await fireEvent.pointerDown(track, {
      button: 0,
      pointerId: 7,
      clientX: 20,
    });
    await fireEvent.pointerMove(track, {
      button: 0,
      pointerId: 7,
      clientX: 130,
    });
    await fireEvent.pointerUp(track, {
      button: 0,
      pointerId: 7,
      clientX: 130,
    });

    expect(onNavigate).toHaveBeenCalled();
    expect(screen.getByRole("button", { name: m.sidebar_clear_selection() })).toBeTruthy();
  });

  it("zooms around the pointer and resets the viewport", async () => {
    const data = fixture();
    render(TimingOverview, { ...data, onNavigate: vi.fn() });
    const track = document.querySelector<HTMLElement>(".overview-track")!;
    track.getBoundingClientRect = () => ({
      x: 0,
      y: 0,
      top: 0,
      right: 200,
      bottom: 64,
      left: 0,
      width: 200,
      height: 64,
      toJSON: () => ({}),
    });

    await fireEvent.wheel(track, { clientX: 100, deltaY: -500 });
    const reset = screen.getByRole("button", {
      name: m.session_vitals_timeline_reset_view(),
    });
    expect(reset).toBeTruthy();

    await fireEvent.click(reset);
    expect(
      screen.queryByRole("button", {
        name: m.session_vitals_timeline_reset_view(),
      }),
    ).toBeNull();
  });

  it("loads one earlier message page from the omitted-history boundary", async () => {
    const data = fixture();
    const onLoadEarlier = vi.fn();
    render(TimingOverview, {
      ...data,
      hasEarlierMessages: true,
      onLoadEarlier,
      onNavigate: vi.fn(),
    });

    await fireEvent.click(
      screen.getByRole("button", {
        name: m.session_vitals_timeline_load_earlier(),
      }),
    );
    expect(onLoadEarlier).toHaveBeenCalledOnce();
  });
});
