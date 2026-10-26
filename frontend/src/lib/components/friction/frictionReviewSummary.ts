import type { DbInsight, GenerateInsightRequest } from "../../api/generated/index";

export const FRICTION_REVIEW_KIND = "friction_review" as const;

// The list endpoint treats dates as range bounds, so match both bounds.
// Results are newest first.
export function pickFrictionReviewInsight(list: DbInsight[], date: string): DbInsight | null {
  return (
    list.find(
      (row) =>
        row.type === "llm_canned" &&
        row.kind === FRICTION_REVIEW_KIND &&
        !row.project &&
        row.date_from === date &&
        row.date_to === date,
    ) ?? null
  );
}

export function frictionReviewRequest(
  date: string,
  agent: string,
  forceRefresh: boolean,
): GenerateInsightRequest {
  return {
    type: "llm_canned",
    kind: FRICTION_REVIEW_KIND,
    llm_opt_in: true,
    date_from: date,
    date_to: date,
    agent,
    ...(forceRefresh ? { force_refresh: true } : {}),
  };
}
