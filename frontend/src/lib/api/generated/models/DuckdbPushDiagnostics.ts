/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DuckdbPushSessionCounts } from './DuckdbPushSessionCounts';
export type DuckdbPushDiagnostics = {
  CandidateSessions: DuckdbPushSessionCounts;
  Cutoff: string;
  DeletedStaleSessions: number;
  Full: boolean;
  LastPushAt: string;
  LocalSessions: DuckdbPushSessionCounts;
  PushedSessions: DuckdbPushSessionCounts;
  SkippedUnchangedSessions: DuckdbPushSessionCounts;
};

