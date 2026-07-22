/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { MoneyMoney } from './MoneyMoney';
export type SessionUsageResponse = {
  agent: string;
  ai_credits?: number;
  breakdown: any[] | null;
  breakdown_count: number;
  cost: MoneyMoney;
  has_cost: boolean;
  has_rollup_cost?: boolean;
  has_token_data: boolean;
  models: any[] | null;
  peak_context_tokens: number;
  project: string;
  rollup_cost?: MoneyMoney;
  rollup_subagent_count?: number;
  server_running: boolean;
  session_id: string;
  total_output_tokens: number;
  unpriced_models: any[] | null;
};

