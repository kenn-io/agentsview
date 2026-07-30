/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { MoneyMoney } from './MoneyMoney';
export type DbTopSessionEntry = {
  agent: string;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  cost: MoneyMoney;
  displayName: string;
  inputTokens: number;
  outputTokens: number;
  project: string;
  sessionId: string;
  startedAt: string;
  totalTokens: number;
};

