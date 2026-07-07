/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DbPeakContextDistribution } from './DbPeakContextDistribution';
import type { DbScopedDistributionPair } from './DbScopedDistributionPair';
export type DbStatsDistributions = {
  duration_minutes: DbScopedDistributionPair;
  peak_context_tokens: DbPeakContextDistribution;
  tools_per_turn: DbScopedDistributionPair;
  user_messages: DbScopedDistributionPair;
};

