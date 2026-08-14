/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { ActivityReport } from '../models/ActivityReport';
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
     * Filter by branch name; multiple names use the branch list separator, and legacy project-qualified values remain accepted
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
}
