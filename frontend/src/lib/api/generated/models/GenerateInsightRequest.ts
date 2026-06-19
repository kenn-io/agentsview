/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { InsightCannedSessionFilters } from './InsightCannedSessionFilters';
export type GenerateInsightRequest = {
  agent?: string;
  automated_scope?: string;
  date_from: string;
  date_to: string;
  filters?: InsightCannedSessionFilters;
  force_refresh?: boolean;
  kind?: string;
  llm_opt_in?: boolean;
  project?: string;
  prompt?: string;
  timezone?: string;
  type: string;
};

