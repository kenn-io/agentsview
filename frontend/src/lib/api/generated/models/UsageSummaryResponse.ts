/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { CacheStats } from './CacheStats';
import type { Comparison } from './Comparison';
import type { DbUsageSessionCounts } from './DbUsageSessionCounts';
import type { DbUsageTotals } from './DbUsageTotals';
import type { ExportPricingBlock } from './ExportPricingBlock';
import type { ExportProjectMapEntry } from './ExportProjectMapEntry';
import type { UnsupportedUsage } from './UnsupportedUsage';
export type UsageSummaryResponse = {
  agentTotals: any[] | null;
  cacheStats: CacheStats;
  comparison?: Comparison;
  daily: any[] | null;
  from: string;
  modelTotals: any[] | null;
  pricing?: ExportPricingBlock;
  projectTotals: any[] | null;
  projects: Record<string, ExportProjectMapEntry>;
  schema_version?: number;
  sessionCounts: DbUsageSessionCounts;
  to: string;
  totals: DbUsageTotals;
  unsupportedUsage?: UnsupportedUsage;
};

