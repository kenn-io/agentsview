import type { DbInsight, GenerateInsightRequest } from "../../api/generated/index";

export const FRICTION_REVIEW_KIND = "friction_review" as const;

// digestRevision reads the digest revision a saved summary was written from.
function digestRevision(row: DbInsight): number | null {
  try {
    const provenance = JSON.parse(row.provenance_json ?? "") as {
      source_versions?: Record<string, string>;
    };
    const value = Number(provenance.source_versions?.friction_digest_revision);
    return Number.isInteger(value) ? value : null;
  } catch {
    return null;
  }
}

// The list endpoint treats dates as range bounds, so match both bounds.
// Results are newest first. A summary of an older revision of the digest is
// out of date and not shown.
export function pickFrictionReviewInsight(
  list: DbInsight[],
  date: string,
  revision: number,
): DbInsight | null {
  return (
    list.find(
      (row) =>
        row.type === "llm_canned" &&
        row.kind === FRICTION_REVIEW_KIND &&
        !row.project &&
        row.date_from === date &&
        row.date_to === date &&
        digestRevision(row) === revision,
    ) ?? null
  );
}

export function frictionReviewRequest(
  date: string,
  agent: string | undefined,
  forceRefresh: boolean,
): GenerateInsightRequest {
  return {
    type: "llm_canned",
    kind: FRICTION_REVIEW_KIND,
    llm_opt_in: true,
    date_from: date,
    date_to: date,
    ...(agent ? { agent } : {}),
    ...(forceRefresh ? { force_refresh: true } : {}),
  };
}
