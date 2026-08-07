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
  cost_source?: string;
  cost_usd?: number;
  has_cost: boolean;
  has_rollup_cost?: boolean;
  has_token_data: boolean;
  models: any[] | null;
  peak_context_tokens: number;
  project: string;
  rollup_cost?: MoneyMoney;
  rollup_cost_source?: string;
  rollup_subagent_count?: number;
  server_running: boolean;
  session_id: string;
  subagent_count?: number;
  total_output_tokens: number;
  unpriced_models: any[] | null;
};
