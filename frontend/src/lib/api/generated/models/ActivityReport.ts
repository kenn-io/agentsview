/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { ActivityPeak } from './ActivityPeak';
import type { ActivityTotals } from './ActivityTotals';
import type { ExportPricingBlock } from './ExportPricingBlock';
import type { ExportProjectMapEntry } from './ExportProjectMapEntry';
export type ActivityReport = {
  as_of: string | null;
  bucket_count: number;
  bucket_seconds: number;
  bucket_unit: string;
  buckets: any[] | null;
  by_agent: any[] | null;
  by_model: any[] | null;
  by_project: any[] | null;
  by_session: any[] | null;
  effective_end: string;
  elapsed_bucket_count: number;
  intervals: any[] | null;
  partial: boolean;
  peak: ActivityPeak;
  pricing?: ExportPricingBlock;
  projects: Record<string, ExportProjectMapEntry>;
  range_end: string;
  range_start: string;
  schema_version?: number;
  timezone: string;
  totals: ActivityTotals;
};

