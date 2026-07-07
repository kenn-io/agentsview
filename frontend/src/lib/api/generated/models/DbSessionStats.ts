/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DbCodeAttribution } from './DbCodeAttribution';
import type { DbStatsAdoption } from './DbStatsAdoption';
import type { DbStatsAgentPortfolio } from './DbStatsAgentPortfolio';
import type { DbStatsArchetypes } from './DbStatsArchetypes';
import type { DbStatsCacheEconomics } from './DbStatsCacheEconomics';
import type { DbStatsDistributions } from './DbStatsDistributions';
import type { DbStatsFilters } from './DbStatsFilters';
import type { DbStatsModelMix } from './DbStatsModelMix';
import type { DbStatsOutcomes } from './DbStatsOutcomes';
import type { DbStatsOutcomeStats } from './DbStatsOutcomeStats';
import type { DbStatsTemporal } from './DbStatsTemporal';
import type { DbStatsToolMix } from './DbStatsToolMix';
import type { DbStatsTotals } from './DbStatsTotals';
import type { DbStatsVelocity } from './DbStatsVelocity';
import type { DbStatsWindow } from './DbStatsWindow';
export type DbSessionStats = {
  adoption?: DbStatsAdoption;
  agent_portfolio: DbStatsAgentPortfolio;
  archetypes: DbStatsArchetypes;
  cache_economics?: DbStatsCacheEconomics;
  code_attribution?: DbCodeAttribution;
  distributions: DbStatsDistributions;
  filters: DbStatsFilters;
  generated_at: string;
  model_mix: DbStatsModelMix;
  outcome_stats?: DbStatsOutcomeStats;
  outcomes?: DbStatsOutcomes;
  schema_version: number;
  temporal: DbStatsTemporal;
  tool_mix: DbStatsToolMix;
  totals: DbStatsTotals;
  velocity: DbStatsVelocity;
  window: DbStatsWindow;
};

