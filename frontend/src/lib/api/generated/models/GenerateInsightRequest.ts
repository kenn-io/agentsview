/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { CannedSessionFiltersInput } from './CannedSessionFiltersInput';
export type GenerateInsightRequest = {
  agent?: string;
  automated_scope?: string;
  date_from: string;
  date_to: string;
  filters?: CannedSessionFiltersInput;
  force_refresh?: boolean;
  kind?: string;
  llm_opt_in?: boolean;
  project?: string;
  prompt?: string;
  session_id?: string;
  timezone?: string;
  type: string;
};

