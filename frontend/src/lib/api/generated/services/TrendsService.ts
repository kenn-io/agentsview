/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DbTrendsTermsResponse } from '../models/DbTrendsTermsResponse';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class TrendsService {
  /**
   * Get trend terms
   * @returns DbTrendsTermsResponse OK
   * @throws ApiError
   */
  public static getApiV1TrendsTerms({
    from,
    to,
    timezone,
    machine,
    project,
    gitBranch,
    agent,
    model,
    dow,
    hour,
    minUserMessages,
    activeSince,
    automatedScope,
    includeOneShot,
    includeAutomated,
    termination,
    term,
    granularity = 'week',
  }: {
    /**
     * Range start date
     */
    from?: string,
    /**
     * Range end date
     */
    to?: string,
    /**
     * IANA timezone name
     */
    timezone?: string,
    /**
     * Filter by machine
     */
    machine?: string,
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
     * Comma-separated model filter
     */
    model?: string,
    /**
     * Day of week, Monday=0 through Sunday=6
     */
    dow?: number,
    /**
     * Hour of day, 0 through 23
     */
    hour?: number,
    /**
     * Minimum user message count
     */
    minUserMessages?: number,
    /**
     * Filter sessions active since this RFC3339 timestamp
     */
    activeSince?: string,
    /**
     * Automation scope
     */
    automatedScope?: 'human' | 'all' | 'automated',
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
    /**
     * Filter by termination reason
     */
    termination?: string,
    /**
     * Terms to trend
     */
    term?: any[] | null,
    /**
     * Time bucket granularity
     */
    granularity?: 'day' | 'week' | 'month',
  }): CancelablePromise<DbTrendsTermsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/trends/terms',
      query: {
        'from': from,
        'to': to,
        'timezone': timezone,
        'machine': machine,
        'project': project,
        'git_branch': gitBranch,
        'agent': agent,
        'model': model,
        'dow': dow,
        'hour': hour,
        'min_user_messages': minUserMessages,
        'active_since': activeSince,
        'automated_scope': automatedScope,
        'include_one_shot': includeOneShot,
        'include_automated': includeAutomated,
        'termination': termination,
        'term': term,
        'granularity': granularity,
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
