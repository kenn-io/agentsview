/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { ExportPricingApplication } from './ExportPricingApplication';
import type { ExportPricingBand } from './ExportPricingBand';
import type { MoneyMoney } from './MoneyMoney';
export type ExportEffectiveModelRate = {
  application: ExportPricingApplication;
  bands: Array<ExportPricingBand> | null;
  cache_read_cost_per_mtok: MoneyMoney;
  cache_write_1h_cost_per_mtok: MoneyMoney;
  cache_write_cost_per_mtok: MoneyMoney;
  cost_source: string;
  input_cost_per_mtok: MoneyMoney;
  matched_pattern: string | null;
  output_cost_per_mtok: MoneyMoney;
  priced_model: string;
};

