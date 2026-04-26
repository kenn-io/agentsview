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

  it("splits sub-agent and non-sub-agent attribution", () => {
    // 3-call parallel turn: 2 reads + 1 sub-agent (2m of the 2m18s turn)
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
    // Returns the dominant non-sub-agent attribution (2 reads → "Read");
    // sub-agent contribution is reported separately via byCategory.
    // Spec says: subtract sub-agent UNION from turn duration; remainder
    // goes to dominant non-sub-agent.
    expect(result).toEqual({ category: "Read", durationMs: 18_000 });
  });

  it("uses the union of overlapping parallel sub-agent ranges", () => {
    // 2 sub-agents in parallel; A=[0,100], B=[50,200]; union=[0,200]=200
    // turn duration = 220, remainder = 20 attributed to dominant non-sub-agent
    // (none here → 'Mixed').
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
    expect(result).toEqual({ category: "Mixed", durationMs: 20 });
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
