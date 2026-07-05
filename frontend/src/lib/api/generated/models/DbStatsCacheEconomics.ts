/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DbCacheHitRatioDistribution } from './DbCacheHitRatioDistribution';
export type DbStatsCacheEconomics = {
  cache_hit_ratio: DbCacheHitRatioDistribution;
  claude_only: boolean;
  dollars_saved_vs_uncached: number;
  dollars_spent: number;
};

