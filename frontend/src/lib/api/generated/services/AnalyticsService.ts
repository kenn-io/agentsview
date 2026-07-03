/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DbActivityResponse } from '../models/DbActivityResponse';
import type { DbAnalyticsSummary } from '../models/DbAnalyticsSummary';
import type { DbHeatmapResponse } from '../models/DbHeatmapResponse';
import type { DbHourOfWeekResponse } from '../models/DbHourOfWeekResponse';
import type { DbProjectsAnalyticsResponse } from '../models/DbProjectsAnalyticsResponse';
import type { DbSessionShapeResponse } from '../models/DbSessionShapeResponse';
import type { DbSignalsAnalyticsResponse } from '../models/DbSignalsAnalyticsResponse';
import type { DbSignalSessionsResponse } from '../models/DbSignalSessionsResponse';
import type { DbSkillsAnalyticsResponse } from '../models/DbSkillsAnalyticsResponse';
import type { DbToolsAnalyticsResponse } from '../models/DbToolsAnalyticsResponse';
import type { DbTopSessionsResponse } from '../models/DbTopSessionsResponse';
import type { DbVelocityResponse } from '../models/DbVelocityResponse';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class AnalyticsService {
  /**
   * Get analytics activity
   * @returns DbActivityResponse OK
   * @throws ApiError
   */
  public static getApiV1AnalyticsActivity({
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
    granularity = 'day',
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
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
     * Time bucket granularity
     */
    granularity?: 'day' | 'week' | 'month',
  }): CancelablePromise<DbActivityResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/analytics/activity',
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
  /**
   * Get analytics heatmap
   * @returns DbHeatmapResponse OK
   * @throws ApiError
   */
  public static getApiV1AnalyticsHeatmap({
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
    metric = 'messages',
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
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
     * Heatmap metric
     */
    metric?: 'messages' | 'sessions' | 'output_tokens',
  }): CancelablePromise<DbHeatmapResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/analytics/heatmap',
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
        'metric': metric,
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
   * Get analytics by hour of week
   * @returns DbHourOfWeekResponse OK
   * @throws ApiError
   */
  public static getApiV1AnalyticsHourOfWeek({
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
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
  }): CancelablePromise<DbHourOfWeekResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/analytics/hour-of-week',
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
   * Get analytics by project
   * @returns DbProjectsAnalyticsResponse OK
   * @throws ApiError
   */
  public static getApiV1AnalyticsProjects({
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
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
  }): CancelablePromise<DbProjectsAnalyticsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/analytics/projects',
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
   * Get session shape analytics
   * @returns DbSessionShapeResponse OK
   * @throws ApiError
   */
  public static getApiV1AnalyticsSessions({
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
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
  }): CancelablePromise<DbSessionShapeResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/analytics/sessions',
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
   * Get signal session examples
   * @returns DbSignalSessionsResponse OK
   * @throws ApiError
   */
  public static getApiV1AnalyticsSignalSessions({
    signal,
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
    limit = 10,
  }: {
    /**
     * Signal name
     */
    signal: string,
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
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
     * Maximum number of session examples
     */
    limit?: number,
  }): CancelablePromise<DbSignalSessionsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/analytics/signal-sessions',
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
        'signal': signal,
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
  /**
   * Get signal analytics
   * @returns DbSignalsAnalyticsResponse OK
   * @throws ApiError
   */
  public static getApiV1AnalyticsSignals({
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
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
  }): CancelablePromise<DbSignalsAnalyticsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/analytics/signals',
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
   * Get skill analytics
   * @returns DbSkillsAnalyticsResponse OK
   * @throws ApiError
   */
  public static getApiV1AnalyticsSkills({
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
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
  }): CancelablePromise<DbSkillsAnalyticsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/analytics/skills',
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
   * Get analytics summary
   * @returns DbAnalyticsSummary OK
   * @throws ApiError
   */
  public static getApiV1AnalyticsSummary({
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
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
  }): CancelablePromise<DbAnalyticsSummary> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/analytics/summary',
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
   * Get tool analytics
   * @returns DbToolsAnalyticsResponse OK
   * @throws ApiError
   */
  public static getApiV1AnalyticsTools({
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
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
  }): CancelablePromise<DbToolsAnalyticsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/analytics/tools',
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
   * Get top sessions
   * @returns DbTopSessionsResponse OK
   * @throws ApiError
   */
  public static getApiV1AnalyticsTopSessions({
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
    metric = 'messages',
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
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
     * Ranking metric
     */
    metric?: 'messages' | 'duration' | 'output_tokens',
  }): CancelablePromise<DbTopSessionsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/analytics/top-sessions',
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
        'metric': metric,
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
   * Get velocity analytics
   * @returns DbVelocityResponse OK
   * @throws ApiError
   */
  public static getApiV1AnalyticsVelocity({
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
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
  }): CancelablePromise<DbVelocityResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/analytics/velocity',
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
