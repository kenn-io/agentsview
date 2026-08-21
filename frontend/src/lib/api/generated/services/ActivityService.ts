/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { ActivityReport } from '../models/ActivityReport';
import type { ActivityReportSessionsResponse } from '../models/ActivityReportSessionsResponse';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class ActivityService {
  /**
   * Get activity report
   * @returns ActivityReport OK
   * @throws ApiError
   */
  public static getApiV1ActivityReport({
    preset,
    date,
    from,
    to,
    timezone,
    bucket,
    project,
    gitBranch,
    agent,
    machine,
    automation = 'all',
  }: {
    /**
     * Range preset
     */
    preset?: 'day' | 'week' | 'month' | 'custom',
    /**
     * Calendar day (YYYY-MM-DD) for presets
     */
    date?: string,
    /**
     * Range start (RFC3339) for custom ranges
     */
    from?: string,
    /**
     * Range end (RFC3339) for custom ranges
     */
    to?: string,
    /**
     * IANA timezone name
     */
    timezone?: string,
    /**
     * Timeline bucket size override
     */
    bucket?: '5m' | '15m' | '1h' | '1d' | '1w',
    /**
     * Filter by project
     */
    project?: string,
    /**
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
     */
    gitBranch?: string,
    /**
     * Filter by agent
     */
    agent?: string,
    /**
     * Filter by machine
     */
    machine?: string,
    /**
     * Automation class: all, interactive, or automated
     */
    automation?: string,
  }): CancelablePromise<ActivityReport> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/activity/report',
      query: {
        'preset': preset,
        'date': date,
        'from': from,
        'to': to,
        'timezone': timezone,
        'bucket': bucket,
        'project': project,
        'git_branch': gitBranch,
        'agent': agent,
        'machine': machine,
        'automation': automation,
      },
      errors: {
        400: `Bad Request`,
        401: `Unauthorized`,
        403: `Forbidden`,
        404: `Not Found`,
        409: `Conflict`,
        422: `Unprocessable Entity`,
        500: `Internal Server Error`,
        501: `Not Implemented`,
        502: `Bad Gateway`,
        503: `Service Unavailable`,
        504: `Gateway Timeout`,
      },
    });
  }
  /**
   * Page activity report sessions
   * @returns ActivityReportSessionsResponse OK
   * @throws ApiError
   */
  public static getApiV1ActivityReportReportIdSessions({
    reportId,
    limit,
    cursor,
    sort,
    direction,
    bucketStart,
    bucketEnd,
    includeReport,
  }: {
    /**
     * Signed Activity report ID
     */
    reportId: string,
    /**
     * Maximum session rows
     */
    limit?: number,
    /**
     * Opaque page cursor
     */
    cursor?: string,
    /**
     * Session sort
     */
    sort?: 'agent_minutes' | 'cost' | 'first_active' | 'project' | 'agent',
    /**
     * Sort direction
     */
    direction?: 'asc' | 'desc',
    /**
     * First timeline bucket in the half-open range
     */
    bucketStart?: number,
    /**
     * Exclusive end of the timeline bucket range
     */
    bucketEnd?: number,
    /**
     * Include full report metadata for stateless clients
     */
    includeReport?: boolean,
  }): CancelablePromise<ActivityReportSessionsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/activity/report/{report_id}/sessions',
      path: {
        'report_id': reportId,
      },
      query: {
        'limit': limit,
        'cursor': cursor,
        'sort': sort,
        'direction': direction,
        'bucket_start': bucketStart,
        'bucket_end': bucketEnd,
        'include_report': includeReport,
      },
      errors: {
        400: `Bad Request`,
        401: `Unauthorized`,
        403: `Forbidden`,
        404: `Not Found`,
        409: `Conflict`,
        422: `Unprocessable Entity`,
        500: `Internal Server Error`,
        501: `Not Implemented`,
        502: `Bad Gateway`,
        503: `Service Unavailable`,
        504: `Gateway Timeout`,
      },
    });
  }
}
