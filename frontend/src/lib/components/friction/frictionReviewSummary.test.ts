import { describe, expect, it } from "vite-plus/test";
import type { DbInsight } from "../../api/generated/index";
import { FRICTION_REVIEW_KIND, pickFrictionReviewInsight } from "./frictionReviewSummary.js";

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
    provenance_json: JSON.stringify({ source_versions: { friction_digest_revision: "3" } }),
    ...overrides,
  } as DbInsight;
}

describe("pickFrictionReviewInsight", () => {
  it.each([
    ["other kind", insight({ kind: "tool_reliability_review" })],
    [
      "older digest revision",
      insight({
        provenance_json: JSON.stringify({ source_versions: { friction_digest_revision: "2" } }),
      }),
    ],
    ["no recorded revision", insight({ provenance_json: "" })],
  ])("ignores %s", (_name, row) => {
    expect(pickFrictionReviewInsight([row], "2025-01-15", 3)).toBeNull();
  });

  it("returns the newest matching row from newest-first lists", () => {
    const latest = insight({ id: 2 });
    expect(pickFrictionReviewInsight([latest, insight()], "2025-01-15", 3)).toBe(latest);
  });
});
