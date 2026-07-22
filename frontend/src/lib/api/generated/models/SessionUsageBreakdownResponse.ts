/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { MoneyMoney } from './MoneyMoney';
export type SessionUsageBreakdownResponse = {
  cache_creation_input_tokens: number;
  cache_read_input_tokens: number;
  cost: MoneyMoney;
  has_cost: boolean;
  input_tokens: number;
  label: string;
  message_ordinal?: number;
  model: string;
  ordinal: number;
  output_tokens: number;
  source: string;
  timestamp: string;
};

