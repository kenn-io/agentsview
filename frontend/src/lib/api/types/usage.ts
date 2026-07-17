/** Usage types — match Go structs in internal/server/usage.go
 *  and internal/db/usage.go */

export interface UsageTotals {
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  totalCost: number;
  copilotAICredits?: number;
}

export interface ModelBreakdown {
  modelName: string;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  cost: number;
}

export interface ProjectBreakdown {
  project_key: string;
  project: string;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  cost: number;
}

export interface AgentBreakdown {
  agent: string;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  cost: number;
}

export interface MachineBreakdown {
  machineName: string;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  cost: number;
}

export interface DailyUsageEntry {
  date: string;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  totalCost: number;
  modelsUsed: string[];
  modelBreakdowns?: ModelBreakdown[];
  projectBreakdowns?: ProjectBreakdown[];
  agentBreakdowns?: AgentBreakdown[];
  machineBreakdowns?: MachineBreakdown[];
}

export interface ProjectTotal {
  project_key: string;
  project: string;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  cost: number;
}

export interface ModelTotal {
  model: string;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  cost: number;
}

export interface AgentTotal {
  agent: string;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  cost: number;
}

export interface CacheStats {
  cacheReadTokens: number;
  cacheCreationTokens: number;
  uncachedInputTokens: number;
  outputTokens: number;
  hitRate: number;
  savingsVsUncached: number;
}

export interface UsageSessionCounts {
  total: number;
  byProject: Record<string, number>;
  byAgent: Record<string, number>;
}

export interface UsageComparison {
  priorFrom: string;
  priorTo: string;
  priorTotalCost: number;
  deltaPct: number;
}

export interface UnsupportedUsage {
  kind: string;
}

export type UsagePairwiseDimension = "model" | "project";

export interface UsagePairwiseComparisonSide {
  totalCost: number;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  totalTokens: number;
  sessionCount: number;
  costPerSession?: number;
  tokensPerSession?: number;
}

export interface UsagePairwiseComparisonDelta {
  totalCostDelta: number;
  totalCostDeltaRatio: number | null;
  inputTokensDelta: number;
  inputTokensDeltaRatio: number | null;
  outputTokensDelta: number;
  outputTokensDeltaRatio: number | null;
  cacheCreationDelta: number;
  cacheCreationDeltaRatio: number | null;
  cacheReadDelta: number;
  cacheReadDeltaRatio: number | null;
  totalTokensDelta: number;
  totalTokensDeltaRatio: number | null;
  sessionCountDelta: number;
  sessionCountDeltaRatio: number | null;
  costPerSessionDelta: number | null;
  costPerSessionRatio: number | null;
  tokensPerSessionDelta: number | null;
  tokensPerSessionRatio: number | null;
}

export interface UsagePairwiseComparisonResponse {
  left: UsagePairwiseComparisonSide;
  right: UsagePairwiseComparisonSide;
  deltas: UsagePairwiseComparisonDelta;
}

export interface UsageSummaryResponse {
  from: string;
  to: string;
  totals: UsageTotals;
  daily: DailyUsageEntry[];
  projectTotals: ProjectTotal[];
  modelTotals: ModelTotal[];
  agentTotals: AgentTotal[];
  sessionCounts: UsageSessionCounts;
  cacheStats: CacheStats;
  unsupportedUsage?: UnsupportedUsage;
  comparison?: UsageComparison;
}

export interface TopSessionEntry {
  sessionId: string;
  displayName: string;
  agent: string;
  project: string;
  startedAt: string;
  totalTokens: number;
  cost: number;
}

export type TopUsageSessionsResponse = TopSessionEntry[];

export interface UsageParams {
  from?: string;
  to?: string;
  project?: string;
  machine?: string;
  agent?: string;
  model?: string;
  exclude_project?: string;
  exclude_agent?: string;
  exclude_model?: string;
  min_user_messages?: number;
  include_one_shot?: boolean;
  include_automated?: boolean;
  active_since?: string;
  timezone?: string;
}

export interface UsageTopSessionsParams extends UsageParams {
  limit?: number;
}
