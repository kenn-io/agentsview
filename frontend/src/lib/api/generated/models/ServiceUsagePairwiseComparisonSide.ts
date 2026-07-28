/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { MoneyMoney } from './MoneyMoney';
export type ServiceUsagePairwiseComparisonSide = {
  cacheCreationTokens: number;
  cacheReadTokens: number;
  costPerSession?: MoneyMoney;
  inputTokens: number;
  outputTokens: number;
  sessionCount: number;
  tokensPerSession?: number;
  totalCost: MoneyMoney;
  totalTokens: number;
};

