import type { ServiceUsagePairwiseComparisonResponse } from "../api/generated/index";
import { testMoney } from "./money.js";

export function emptyUsagePairwiseComparison(): ServiceUsagePairwiseComparisonResponse {
  const side = {
    totalCost: testMoney(0),
    inputTokens: 0,
    outputTokens: 0,
    cacheCreationTokens: 0,
    cacheReadTokens: 0,
    totalTokens: 0,
    sessionCount: 0,
  };
  return {
    left: side,
    right: side,
    deltas: {
      totalCostDelta: testMoney(0),
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
