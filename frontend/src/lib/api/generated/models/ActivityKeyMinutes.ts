/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { MoneyMoney } from './MoneyMoney';
export type ActivityKeyMinutes = {
  agent_minutes: number;
  automated_agent_minutes: number;
  automated_cost: MoneyMoney;
  cost: MoneyMoney;
  interactive_agent_minutes: number;
  interactive_cost: MoneyMoney;
  key: string;
  project_key?: string;
};

