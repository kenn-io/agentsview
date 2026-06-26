/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DbSignalCalibration } from './DbSignalCalibration';
import type { DbSignalsContextHealth } from './DbSignalsContextHealth';
import type { DbSignalsQualityHealth } from './DbSignalsQualityHealth';
import type { DbSignalsToolHealth } from './DbSignalsToolHealth';
export type DbSignalsAnalyticsResponse = {
  avg_health_score: number | null;
  by_agent: any[] | null;
  by_project: any[] | null;
  calibration: Record<string, DbSignalCalibration>;
  context_health: DbSignalsContextHealth;
  grade_distribution: Record<string, number>;
  outcome_confidence_distribution: Record<string, number>;
  outcome_distribution: Record<string, number>;
  quality_health: DbSignalsQualityHealth;
  scored_sessions: number;
  tool_health: DbSignalsToolHealth;
  trend: any[] | null;
  unscored_sessions: number;
};

