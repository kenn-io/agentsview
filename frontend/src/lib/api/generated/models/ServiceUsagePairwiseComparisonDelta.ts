/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { MoneyMoney } from './MoneyMoney';
export type ServiceUsagePairwiseComparisonDelta = {
  cacheCreationDelta: number;
  cacheCreationDeltaRatio: number | null;
  cacheReadDelta: number;
  cacheReadDeltaRatio: number | null;
  costPerSessionDelta: MoneyMoney;
  costPerSessionRatio: number | null;
  inputTokensDelta: number;
  inputTokensDeltaRatio: number | null;
  outputTokensDelta: number;
  outputTokensDeltaRatio: number | null;
  sessionCountDelta: number;
  sessionCountDeltaRatio: number | null;
  tokensPerSessionDelta: number | null;
  tokensPerSessionRatio: number | null;
  totalCostDelta: MoneyMoney;
  totalCostDeltaRatio: number | null;
  totalTokensDelta: number;
  totalTokensDeltaRatio: number | null;
};

