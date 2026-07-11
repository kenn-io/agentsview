/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { VectorBuildResult } from './VectorBuildResult';
export type VectorBuildStatus = {
  build_id?: number;
  dimension?: number;
  done: number;
  last_error?: string;
  last_result?: VectorBuildResult;
  model?: string;
  phase?: string;
  running: boolean;
  started_at?: string;
  total: number;
};

