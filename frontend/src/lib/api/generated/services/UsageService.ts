/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { Comparison } from '../models/Comparison';
import type { UsageSummaryResponse } from '../models/UsageSummaryResponse';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class UsageService {
  /**
   * Get usage comparison
   * @returns Comparison OK
   * @throws ApiError
   */
  public static getApiV1UsageComparison({
    currentCost,
    from,
    to,
    timezone,
    agent,
    project,
    machine,
    excludeProject,
    excludeAgent,
    excludeModel,
    model,
    minUserMessages,
    activeSince,
    includeOneShot = true,
    includeAutomated,
  }: {
    /**
     * Current period total cost
     */
    currentCost: number,
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
     * Filter by agent
     */
    agent?: string,
    /**
     * Filter by project
     */
    project?: string,
    /**
     * Filter by machine
     */
    machine?: string,
    /**
     * Exclude a project
     */
    excludeProject?: string,
    /**
     * Exclude an agent
     */
    excludeAgent?: string,
    /**
     * Exclude a model
     */
    excludeModel?: string,
    /**
     * Filter by model
     */
    model?: string,
    /**
     * Minimum user message count
     */
    minUserMessages?: number,
    /**
     * Filter sessions active since this RFC3339 timestamp
     */
    activeSince?: string,
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
  }): CancelablePromise<Comparison> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/usage/comparison',
      query: {
        'from': from,
        'to': to,
        'timezone': timezone,
        'agent': agent,
        'project': project,
        'machine': machine,
        'exclude_project': excludeProject,
        'exclude_agent': excludeAgent,
        'exclude_model': excludeModel,
        'model': model,
        'min_user_messages': minUserMessages,
        'active_since': activeSince,
        'include_one_shot': includeOneShot,
        'include_automated': includeAutomated,
        'current_cost': currentCost,
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
   * Get usage summary
   * @returns UsageSummaryResponse OK
   * @throws ApiError
   */
  public static getApiV1UsageSummary({
    from,
    to,
    timezone,
    agent,
    project,
    machine,
    excludeProject,
    excludeAgent,
    excludeModel,
    model,
    minUserMessages,
    activeSince,
    includeOneShot = true,
    includeAutomated,
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
     * Filter by agent
     */
    agent?: string,
    /**
     * Filter by project
     */
    project?: string,
    /**
     * Filter by machine
     */
    machine?: string,
    /**
     * Exclude a project
     */
    excludeProject?: string,
    /**
     * Exclude an agent
     */
    excludeAgent?: string,
    /**
     * Exclude a model
     */
    excludeModel?: string,
    /**
     * Filter by model
     */
    model?: string,
    /**
     * Minimum user message count
     */
    minUserMessages?: number,
    /**
     * Filter sessions active since this RFC3339 timestamp
     */
    activeSince?: string,
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
  }): CancelablePromise<UsageSummaryResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/usage/summary',
      query: {
        'from': from,
        'to': to,
        'timezone': timezone,
        'agent': agent,
        'project': project,
        'machine': machine,
        'exclude_project': excludeProject,
        'exclude_agent': excludeAgent,
        'exclude_model': excludeModel,
        'model': model,
        'min_user_messages': minUserMessages,
        'active_since': activeSince,
        'include_one_shot': includeOneShot,
        'include_automated': includeAutomated,
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
   * Get top usage sessions
   * @returns any[] OK
   * @throws ApiError
   */
  public static getApiV1UsageTopSessions({
    from,
    to,
    timezone,
    agent,
    project,
    machine,
    excludeProject,
    excludeAgent,
    excludeModel,
    model,
    minUserMessages,
    activeSince,
    includeOneShot = true,
    includeAutomated,
    limit = 20,
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
     * Filter by agent
     */
    agent?: string,
    /**
     * Filter by project
     */
    project?: string,
    /**
     * Filter by machine
     */
    machine?: string,
    /**
     * Exclude a project
     */
    excludeProject?: string,
    /**
     * Exclude an agent
     */
    excludeAgent?: string,
    /**
     * Exclude a model
     */
    excludeModel?: string,
    /**
     * Filter by model
     */
    model?: string,
    /**
     * Minimum user message count
     */
    minUserMessages?: number,
    /**
     * Filter sessions active since this RFC3339 timestamp
     */
    activeSince?: string,
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
    /**
     * Maximum number of sessions
     */
    limit?: number,
  }): CancelablePromise<any[] | null> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/usage/top-sessions',
      query: {
        'from': from,
        'to': to,
        'timezone': timezone,
        'agent': agent,
        'project': project,
        'machine': machine,
        'exclude_project': excludeProject,
        'exclude_agent': excludeAgent,
        'exclude_model': excludeModel,
        'model': model,
        'min_user_messages': minUserMessages,
        'active_since': activeSince,
        'include_one_shot': includeOneShot,
        'include_automated': includeAutomated,
        'limit': limit,
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
