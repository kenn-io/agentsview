/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { ActivityPeak } from './ActivityPeak';
import type { ActivityTotals } from './ActivityTotals';
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
  range_end: string;
  range_start: string;
  timezone: string;
  totals: ActivityTotals;
};

