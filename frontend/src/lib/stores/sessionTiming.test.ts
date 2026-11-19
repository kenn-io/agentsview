import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { sessionTiming } from "./sessionTiming.svelte.js";
import type { DbSessionTiming } from "../api/generated/index.js";

const timingMocks = vi.hoisted(() => ({
  fetchSessionTiming: vi.fn(),
}));

vi.mock("../api/generated/sessions/sessions.js", () => ({
  getApiV1SessionsByIdTiming: timingMocks.fetchSessionTiming,
}));

beforeEach(() => {
  sessionTiming.reset();
  timingMocks.fetchSessionTiming.mockReset();
});

describe("SessionTimingStore", () => {
  it("rebuilds the timing index when the snapshot is replaced", () => {
    const oldCall = {
      tool_use_id: "call-1",
      tool_name: "Bash",
      category: "Bash",
      duration_ms: 2000,
      is_parallel: false,
      input_preview: "pwd",
    };
    const oldTurn = {
      message_id: 1,
      ordinal: 1,
      started_at: "2026-07-11T12:00:00Z",
      duration_ms: 2000,
      primary_category: "Bash",
      calls: [oldCall],
    };
    const timing: DbSessionTiming = {
      session_id: "s1",
      total_duration_ms: 6000,
      tool_duration_ms: 2000,
      turn_count: 1,
      tool_call_count: 1,
      subagent_count: 0,
      slowest_call: oldCall,
      by_category: [{ category: "Bash", call_count: 1, duration_ms: 2000 }],
      activity: [
        {
          message_id: 1,
          ordinal: 1,
          started_at: "2026-07-11T12:00:00Z",
          duration_ms: 6000,
          tool_ms: 2000,
          unattributed_ms: 4000,
          running: false,
        },
      ],
      activity_totals: { tool_ms: 2000, unattributed_ms: 4000 },
      running: false,
      turns: [oldTurn],
    };
    sessionTiming.timing = timing;

    expect(sessionTiming.index.turnByMessage.get(1)).toEqual(oldTurn);
    expect(sessionTiming.index.callByToolUseID.get("call-1")).toEqual(oldCall);

    const newCall = { ...oldCall, tool_use_id: "call-2", duration_ms: 3000 };
    const newTurn = { ...oldTurn, message_id: 2, duration_ms: 3000, calls: [newCall] };
    sessionTiming.timing = {
      ...timing,
      tool_duration_ms: 3000,
      slowest_call: newCall,
      by_category: [{ category: "Bash", call_count: 1, duration_ms: 3000 }],
      activity: [{ ...timing.activity[0]!, message_id: 2, tool_ms: 3000, unattributed_ms: 3000 }],
      activity_totals: { tool_ms: 3000, unattributed_ms: 3000 },
      turns: [newTurn],
    };

    expect(sessionTiming.index.turnByMessage.has(1)).toBe(false);
    expect(sessionTiming.index.callByToolUseID.has("call-1")).toBe(false);
    expect(sessionTiming.index.turnByMessage.get(2)).toEqual(newTurn);
    expect(sessionTiming.index.callByToolUseID.get("call-2")).toEqual(newCall);
  });

  it("aborts timing when another session replaces it", async () => {
    const signals: AbortSignal[] = [];
    timingMocks.fetchSessionTiming.mockImplementation(({ id }, { signal }) => {
      signals.push(signal as AbortSignal);
      if (id === "s1") return new Promise(() => {});
      return Promise.resolve({ session_id: id });
    });

    void sessionTiming.load("s1");
    await Promise.resolve();
    await sessionTiming.load("s2");

    expect(signals[0]?.aborted).toBe(true);
  });

  it("aborts the current timing read on route exit", async () => {
    const signals: AbortSignal[] = [];
    timingMocks.fetchSessionTiming.mockImplementation((_params, { signal }) => {
      signals.push(signal as AbortSignal);
      return new Promise(() => {});
    });

    void sessionTiming.load("s1");
    await Promise.resolve();
    sessionTiming.cancelInFlight();

    expect(signals[0]?.aborted).toBe(true);
  });
});
