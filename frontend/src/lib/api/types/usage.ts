/** Frontend usage domain types. Wire shapes for the usage endpoints come
 *  from the generated client (UsageSummaryResponse, Comparison,
 *  ServiceUsagePairwiseComparisonResponse, DbTopSessionEntry). */

/** Dimensions the pairwise comparison panel can compare. The server accepts
 *  a free-form string; the UI only offers these two. */
export type UsagePairwiseDimension = "model" | "project";
