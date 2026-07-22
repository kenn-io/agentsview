/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { MoneyMoney } from './MoneyMoney';
export type ActivityTotals = {
  active_minutes: number;
  agent_minutes: number;
  automated_agent_minutes: number;
  automated_cost: MoneyMoney;
  automated_sessions: number;
  cost: MoneyMoney;
  distinct_models: number;
  distinct_projects: number;
  idle_minutes: number;
  interactive_agent_minutes: number;
  interactive_cost: MoneyMoney;
  interactive_sessions: number;
  output_tokens: number;
  sessions: number;
  untimed_sessions: number;
};

