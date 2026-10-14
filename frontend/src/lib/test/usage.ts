import type { ServiceUsagePairwiseComparisonResponse } from "../api/generated/index";

export function emptyUsagePairwiseComparison(): ServiceUsagePairwiseComparisonResponse {
  const zero = { microdollars: 0 };
  const metrics = {
    totalCost: zero,
    inputTokens: 0,
    outputTokens: 0,
    cacheCreationTokens: 0,
    cacheReadTokens: 0,
    totalTokens: 0,
    sessionCount: 0,
    costPerSession: null,
    tokensPerSession: null,
  };
  return {
    left: { ...metrics },
    right: { ...metrics },
    deltas: {
      totalCostDelta: zero,
      totalCostDeltaRatio: null,
      inputTokensDelta: 0,
      inputTokensDeltaRatio: null,
      outputTokensDelta: 0,
      outputTokensDeltaRatio: null,
      cacheCreationDelta: 0,
      cacheCreationDeltaRatio: null,
      cacheReadDelta: 0,
      cacheReadDeltaRatio: null,
      totalTokensDelta: 0,
      totalTokensDeltaRatio: null,
      sessionCountDelta: 0,
      sessionCountDeltaRatio: null,
      costPerSessionDelta: null,
      costPerSessionRatio: null,
      tokensPerSessionDelta: null,
      tokensPerSessionRatio: null,
    },
  };
}
