import { describe, expect, it } from "vite-plus/test";
import type { DbInsight } from "../../api/generated/index";
import {
  FRICTION_REVIEW_KIND,
  frictionReviewRequest,
  pickFrictionReviewInsight,
} from "./frictionReviewSummary.js";

function insight(overrides: Partial<DbInsight> = {}): DbInsight {
  return {
    id: 1,
    type: "llm_canned",
    kind: FRICTION_REVIEW_KIND,
    date_from: "2025-01-15",
    date_to: "2025-01-15",
    project: null,
    agent: "claude",
    model: null,
    prompt: null,
    content: "Summary",
    created_at: "2025-01-16T00:00:00Z",
    ...overrides,
  } as DbInsight;
}

describe("pickFrictionReviewInsight", () => {
  it.each([
    ["other kind", insight({ kind: "tool_reliability_review" })],
    ["other date", insight({ date_from: "2025-01-14", date_to: "2025-01-14" })],
    ["containing range", insight({ date_from: "2025-01-14" })],
    ["project scope", insight({ project: "p1" })],
    ["other type", insight({ type: "daily_activity" })],
  ])("ignores %s", (_name, row) => {
    expect(pickFrictionReviewInsight([row], "2025-01-15")).toBeNull();
  });

  it("returns the newest matching row from newest-first lists", () => {
    const latest = insight({ id: 2 });
    expect(pickFrictionReviewInsight([latest, insight()], "2025-01-15")).toBe(latest);
  });
});

describe("frictionReviewRequest", () => {
  it("opts in for one date without project or filters", () => {
    expect(frictionReviewRequest("2025-01-15", "codex", false)).toEqual({
      type: "llm_canned",
      kind: "friction_review",
      llm_opt_in: true,
      date_from: "2025-01-15",
      date_to: "2025-01-15",
      agent: "codex",
    });
  });

  it("forces refresh only when regenerating", () => {
    expect(frictionReviewRequest("2025-01-15", "claude", true).force_refresh).toBe(true);
  });
});
