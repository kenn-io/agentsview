import { describe, expect, it } from "vitest";
import {
  attributeTurn,
  type TurnAttributionInput,
} from "./categoryAttribution.js";

function turn(p: Partial<TurnAttributionInput> = {}): TurnAttributionInput {
  return {
    turnDurationMs: 10_000,
    calls: [],
    ...p,
  };
}

describe("attributeTurn", () => {
  it("returns null when turn duration is null (running)", () => {
    expect(attributeTurn(turn({ turnDurationMs: null }))).toBeNull();
  });

  it("attributes a solo turn to the call's category", () => {
    expect(
      attributeTurn(turn({
        turnDurationMs: 5000,
        calls: [{ category: "Bash", durationMs: 5000, isSubagent: false }],
      })),
    ).toEqual({ category: "Bash", durationMs: 5000 });
  });

  it("attributes a parallel turn to the strict majority category", () => {
    expect(
      attributeTurn(turn({
        turnDurationMs: 1400,
        calls: [
          { category: "Read", durationMs: null, isSubagent: false },
          { category: "Read", durationMs: null, isSubagent: false },
          { category: "Read", durationMs: null, isSubagent: false },
          { category: "Bash", durationMs: null, isSubagent: false },
        ],
      })),
    ).toEqual({ category: "Read", durationMs: 1400 });
  });

  it("attributes a non-dominated parallel turn to Mixed", () => {
    expect(
      attributeTurn(turn({
        turnDurationMs: 2000,
        calls: [
          { category: "Read", durationMs: null, isSubagent: false },
          { category: "Bash", durationMs: null, isSubagent: false },
        ],
      })),
    ).toEqual({ category: "Mixed", durationMs: 2000 });
  });

  it("attributes a sub-agent-dominated turn to Task", () => {
    // 3-call parallel turn: 2 reads + 1 sub-agent (2m of the 2m18s turn).
    // Sub-agent union (120s) >= remainder (18s) → Task dominates.
    const result = attributeTurn(turn({
      turnDurationMs: 138_000,
      calls: [
        { category: "Read", durationMs: null, isSubagent: false },
        { category: "Read", durationMs: null, isSubagent: false },
        {
          category: "Task",
          durationMs: 120_000,
          isSubagent: true,
          subagentRange: { startedAtMs: 0, endedAtMs: 120_000 },
        },
      ],
    }));
    expect(result).toEqual({ category: "Task", durationMs: 18_000 });
  });

  it("falls through to non-sub-agent majority when sub-agent doesn't dominate", () => {
    // Sub-agent union 30s, non-sub-agent remainder 90s → Read wins
    // strict majority among the 3 non-sub Reads.
    const result = attributeTurn(turn({
      turnDurationMs: 120_000,
      calls: [
        { category: "Read", durationMs: null, isSubagent: false },
        { category: "Read", durationMs: null, isSubagent: false },
        { category: "Read", durationMs: null, isSubagent: false },
        {
          category: "Task",
          durationMs: 30_000,
          isSubagent: true,
          subagentRange: { startedAtMs: 0, endedAtMs: 30_000 },
        },
      ],
    }));
    expect(result).toEqual({ category: "Read", durationMs: 90_000 });
  });

  it("uses the union of overlapping parallel sub-agent ranges", () => {
    // 2 sub-agents in parallel; A=[0,100], B=[50,200]; union=[0,200]=200
    // turn duration = 220, remainder = 20. subUnion (200) >= remainder (20)
    // → Task dominates.
    const result = attributeTurn(turn({
      turnDurationMs: 220,
      calls: [
        {
          category: "Task",
          durationMs: 100,
          isSubagent: true,
          subagentRange: { startedAtMs: 0, endedAtMs: 100 },
        },
        {
          category: "Task",
          durationMs: 150,
          isSubagent: true,
          subagentRange: { startedAtMs: 50, endedAtMs: 200 },
        },
      ],
    }));
    expect(result).toEqual({ category: "Task", durationMs: 20 });
  });

  it("treats Read/Grep/Glob as non-distinct for dominance? No — exact strings", () => {
    // Per spec, attribution operates on exact normalized category values.
    // The frontend color map treats Read/Grep/Glob as one color, but
    // attribution is per category.
    expect(
      attributeTurn(turn({
        turnDurationMs: 500,
        calls: [
          { category: "Read", durationMs: null, isSubagent: false },
          { category: "Grep", durationMs: null, isSubagent: false },
        ],
      })),
    ).toEqual({ category: "Mixed", durationMs: 500 });
  });
});
