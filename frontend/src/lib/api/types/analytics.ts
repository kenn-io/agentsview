/** Analytics types — match Go structs in internal/db/analytics.go */

export type Granularity = "day" | "week" | "month";
export type TrendsGranularity = "day" | "week" | "month";
export type HeatmapMetric = "messages" | "sessions" | "output_tokens";
export type TopSessionsMetric = "messages" | "duration" | "output_tokens";
export type AutomatedScope = "human" | "all" | "automated";
