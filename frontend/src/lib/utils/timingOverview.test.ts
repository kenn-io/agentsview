import { describe, expect, it } from "vite-plus/test";
import type { Message } from "../api/types.js";
import type { SessionTiming } from "../api/types/timing.js";
import {
  deriveTimingOverview,
  projectTimingOverview,
  nearestTimingOverviewSpan,
  orderedTimingOverviewRange,
  timingOverviewFocusOrdinals,
} from "./timingOverview.js";

function message(overrides: Partial<Message>): Message {
  return {
    id: 1,
    session_id: "session-1",
    ordinal: 0,
    role: "user",
    content: "",
    timestamp: "2026-08-30T10:00:00.000Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: 0,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
    ...overrides,
  };
}

function timing(): SessionTiming {
  return {
    session_id: "session-1",
    total_duration_ms: 10_000,
    tool_duration_ms: 4_000,
    turn_count: 1,
    tool_call_count: 1,
    subagent_count: 0,
    slowest_call: null,
    by_category: [],
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
}

describe("deriveTimingOverview", () => {
  it("projects input, model, and exact tool execution onto one clock", () => {
    const messages = [
      message({ ordinal: 0 }),
      message({
        id: 2,
        ordinal: 1,
        role: "assistant",
        timestamp: "2026-08-30T10:00:02.000Z",
        model: "deepseek-v3.2",
        has_tool_use: true,
        tool_calls: [
          {
            tool_use_id: "call-1",
            tool_name: "exec_command",
            category: "Bash",
            result_events: [
              {
                tool_use_id: "call-1",
                source: "tool_result",
                status: "started",
                content: "",
                content_length: 0,
                timestamp: "2026-08-30T10:00:02.050Z",
                event_index: -1,
              },
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
    ];

    const model = deriveTimingOverview(messages, timing(), {
      sessionStartedAt: "2026-08-30T10:00:00.000Z",
      sessionEndedAt: "2026-08-30T10:00:10.000Z",
    });

    expect(model).not.toBeNull();
    expect(model!.endMs - model!.startMs).toBe(10_000);
    expect(model?.spans.map((span) => span.lane)).toEqual(["input", "model", "tools"]);
    expect(model?.spans[1]).toMatchObject({
      startMs: Date.parse("2026-08-30T10:00:00.000Z"),
      endMs: Date.parse("2026-08-30T10:00:02.000Z"),
      ordinal: 1,
      approximate: false,
    });
    expect(model?.spans[2]).toMatchObject({
      startMs: Date.parse("2026-08-30T10:00:02.250Z"),
      endMs: Date.parse("2026-08-30T10:00:05.750Z"),
      approximate: false,
      errored: false,
    });
  });

  it("switches between idle-compressed duration and equal-width sequence space", () => {
    const source = deriveTimingOverview(
      [
        message({ ordinal: 0 }),
        message({
          id: 2,
          ordinal: 1,
          role: "assistant",
          timestamp: "2026-08-30T10:00:02.000Z",
          has_tool_use: true,
        }),
      ],
      timing(),
      { sessionEndedAt: "2026-08-30T10:00:10.000Z" },
    )!;

    const duration = projectTimingOverview(source, "duration");
    expect(duration.endMs - duration.startMs).toBe(6_000);
    expect(duration.spans[1]?.recordedStartMs).toBe(Date.parse("2026-08-30T10:00:00.000Z"));

    const sequence = projectTimingOverview(source, "sequence");
    expect(sequence).toMatchObject({ startMs: 0, endMs: 3 });
    expect(sequence.spans.every((span) => span.endMs - span.startMs === 1)).toBe(true);
  });

  it("marks turn-level tool bounds as approximate without inventing running time", () => {
    const fallbackTiming = timing();
    fallbackTiming.running = true;
    fallbackTiming.turns[0]!.duration_ms = null;
    fallbackTiming.turns[0]!.calls[0]!.duration_ms = null;

    const model = deriveTimingOverview(
      [
        message({ ordinal: 0 }),
        message({
          id: 2,
          ordinal: 1,
          role: "assistant",
          timestamp: "2026-08-30T10:00:02.000Z",
          has_tool_use: true,
        }),
      ],
      fallbackTiming,
      {
        nowMs: Date.parse("2026-08-30T10:00:08.000Z"),
      },
    );
    const tool = model?.spans.find((span) => span.lane === "tools");

    expect(tool).toMatchObject({
      approximate: true,
      running: true,
    });
    expect(tool?.endMs).toBe(tool?.startMs);
    expect(model?.endMs).toBe(Date.parse("2026-08-30T10:00:08.000Z"));
  });

  it("does not project unloaded timing turns before the loaded history window", () => {
    const pagedTiming = timing();
    pagedTiming.turns.unshift({
      ...pagedTiming.turns[0]!,
      message_id: 1,
      ordinal: 1,
    });
    pagedTiming.turns[1] = {
      ...pagedTiming.turns[1]!,
      message_id: 50,
      ordinal: 50,
      started_at: "2026-08-30T11:00:02.000Z",
    };

    const model = deriveTimingOverview(
      [
        message({
          id: 50,
          ordinal: 50,
          role: "assistant",
          timestamp: "2026-08-30T11:00:02.000Z",
          has_tool_use: true,
        }),
      ],
      pagedTiming,
      { hasEarlierMessages: true },
    );

    expect(model?.spans.filter((span) => span.lane === "tools")).toHaveLength(1);
    expect(model?.spans.every((span) => span.ordinal === 50)).toBe(true);
  });
});

describe("timing overview range focus", () => {
  it("returns unique ordered ordinals intersecting an inclusive range", () => {
    const model = deriveTimingOverview(
      [
        message({ ordinal: 0 }),
        message({
          id: 2,
          ordinal: 1,
          role: "assistant",
          timestamp: "2026-08-30T10:00:02.000Z",
        }),
      ],
      timing(),
    )!;
    const range = orderedTimingOverviewRange(
      Date.parse("2026-08-30T10:00:05.000Z"),
      Date.parse("2026-08-30T10:00:01.000Z"),
    );

    expect(timingOverviewFocusOrdinals(model, range)).toEqual([1]);
    expect(nearestTimingOverviewSpan(model, Date.parse("2026-08-30T10:00:00.100Z")).ordinal).toBe(
      1,
    );
  });
});
