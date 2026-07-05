/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { ExportEffectiveModelRate } from './ExportEffectiveModelRate';
import type { ExportPricingFallback } from './ExportPricingFallback';
export type ExportPricingBlock = {
  cost_source: string;
  custom_override_count: number;
  digest: string;
  effective_row_count: number;
  fallback: ExportPricingFallback;
  latest_row_updated_at: string | null;
  models: Record<string, ExportEffectiveModelRate>;
  source: string;
  table_version: string;
};

