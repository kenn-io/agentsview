import { describe, expect, it } from "vite-plus/test";
import type { DbSessionTiming as SessionTiming } from "../api/generated/index.js";
import { callsHaveTiming, turnHasCategory } from "./timing.js";

describe("turnHasCategory", () => {
  it("uses call categories instead of the summary category", () => {
    const turn = {
      message_id: 1,
      ordinal: 0,
      started_at: "2026-09-14T00:00:00Z",
      duration_ms: 1000,
      primary_category: "Mixed",
      calls: [
        {
          tool_use_id: "bash",
          tool_name: "Bash",
          category: "Bash",
          duration_ms: 1000,
          is_parallel: true,
          input_preview: "pwd",
        },
        {
          tool_use_id: "read",
          tool_name: "Read",
          category: "Read",
          duration_ms: null,
          is_parallel: true,
          input_preview: "main.go",
        },
      ],
    };

    expect(turnHasCategory(turn, "Read")).toBe(true);
    expect(turnHasCategory(turn, "Mixed")).toBe(false);
  });
});

describe("callsHaveTiming", () => {
  function timingWith(durations: (number | null)[], running: boolean): SessionTiming {
    return {
      session_id: "s",
      total_duration_ms: 1000,
      tool_duration_ms: 0,
      turn_count: 1,
      tool_call_count: durations.length,
      subagent_count: 0,
      slowest_call: null,
      by_category: [],
      activity: [],
      activity_totals: { tool_ms: 0, unattributed_ms: 0 },
      running,
      turns: [
        {
          message_id: 1,
          ordinal: 0,
          started_at: "2026-09-14T00:00:00Z",
          duration_ms: running ? null : 1000,
          primary_category: "Bash",
          calls: durations.map((duration_ms, i) => ({
            tool_use_id: `c${i}`,
            tool_name: "Bash",
            category: "Bash",
            duration_ms,
            is_parallel: durations.length > 1,
            input_preview: "pwd",
          })),
        },
      ],
    };
  }

  it.each([
    { name: "some calls have durations", durations: [null, 0], running: false, want: true },
    { name: "no call has a duration", durations: [null, null], running: false, want: false },
    { name: "a running call without durations", durations: [null], running: true, want: true },
  ])("is $want when $name", ({ durations, running, want }) => {
    expect(callsHaveTiming(timingWith(durations, running))).toBe(want);
  });
});
