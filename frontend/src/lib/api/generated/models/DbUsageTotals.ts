/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { MoneyMoney } from './MoneyMoney';
export type DbUsageTotals = {
  cacheCreationTokens: number;
  cacheReadTokens: number;
  cacheSavings: MoneyMoney;
  copilotAICredits?: number;
  inputTokens: number;
  outputTokens: number;
  totalCost: MoneyMoney;
};

