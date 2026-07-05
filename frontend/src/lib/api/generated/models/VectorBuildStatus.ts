/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { VectorBuildResult } from './VectorBuildResult';
export type VectorBuildStatus = {
  done: number;
  last_error?: string;
  last_result?: VectorBuildResult;
  phase?: string;
  running: boolean;
  total: number;
};

