/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { ActivityReport } from './ActivityReport';
export type ActivityReportSessionsResponse = {
  next_cursor?: string;
  refresh_required?: boolean;
  report?: ActivityReport;
  report_id: string;
  sessions: any[] | null;
  total: number;
};
