/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { MoneyMoney } from './MoneyMoney';
export type DbBranchBreakdown = {
  branch: string;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  cost: MoneyMoney;
  inputTokens: number;
  outputTokens: number;
  project: string;
  project_key: string;
};
