/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DbCacheHitRatioDistribution } from './DbCacheHitRatioDistribution';
import type { MoneyMoney } from './MoneyMoney';
export type DbStatsCacheEconomics = {
  cache_hit_ratio: DbCacheHitRatioDistribution;
  claude_only: boolean;
  saved_vs_uncached: MoneyMoney;
  spent: MoneyMoney;
};

