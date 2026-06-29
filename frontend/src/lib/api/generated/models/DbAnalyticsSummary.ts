/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DbAgentSummary } from './DbAgentSummary';
export type DbAnalyticsSummary = {
  active_days: number;
  active_projects: number;
  agents: Record<string, DbAgentSummary>;
  avg_messages: number;
  concentration: number;
  median_messages: number;
  models: any[] | null;
  most_active_project: string;
  p90_messages: number;
  token_reporting_sessions: number;
  total_messages: number;
  total_output_tokens: number;
  total_sessions: number;
};

