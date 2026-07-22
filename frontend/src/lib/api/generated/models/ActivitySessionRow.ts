/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { MoneyMoney } from './MoneyMoney';
export type ActivitySessionRow = {
  agent: string;
  agent_minutes: number | null;
  cost: MoneyMoney;
  first_active: string | null;
  is_automated: boolean;
  last_active: string | null;
  models: any[] | null;
  output_tokens: number;
  primary_model: string;
  project: string;
  project_key: string;
  session_id: string;
  timing_quality: string;
  title: string;
};

