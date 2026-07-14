/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { FillStats } from './FillStats';
import type { VectorRefreshStats } from './VectorRefreshStats';
import type { VectorRepairStats } from './VectorRepairStats';
export type VectorBuildResult = {
  Activated: boolean;
  Fill: FillStats;
  Fingerprint: string;
  Refresh: VectorRefreshStats;
  Repair: VectorRepairStats;
};

