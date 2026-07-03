/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { Comparison } from '../models/Comparison';
import type { ServiceUsagePairwiseComparisonResponse } from '../models/ServiceUsagePairwiseComparisonResponse';
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
    gitBranch,
    excludeGitBranch,
    excludeProject,
    excludeProjectKey,
    excludeAgent,
    excludeModel,
    model,
    minUserMessages,
    activeSince,
    termination,
    includeOneShot = true,
    includeAutomated,
    noDefaultRange,
    breakdowns = true,
    sessionCounts = true,
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
     */
    gitBranch?: string,
    /**
     * Exclude a git branch; opaque (project, branch) tokens from the /branches endpoint
     */
    excludeGitBranch?: string,
    /**
     * Exclude a project
     */
    excludeProject?: string,
    /**
     * Exclude an opaque project key
     */
    excludeProjectKey?: string,
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
     * Filter by termination status
     */
    termination?: string,
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
    /**
     * Preserve omitted from/to without applying default range
     */
    noDefaultRange?: boolean,
    /**
     * Include per-model, per-project, and per-agent breakdowns
     */
    breakdowns?: boolean,
    /**
     * Include distinct session counts
     */
    sessionCounts?: boolean,
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
        'git_branch': gitBranch,
        'exclude_git_branch': excludeGitBranch,
        'exclude_project': excludeProject,
        'exclude_project_key': excludeProjectKey,
        'exclude_agent': excludeAgent,
        'exclude_model': excludeModel,
        'model': model,
        'min_user_messages': minUserMessages,
        'active_since': activeSince,
        'termination': termination,
        'include_one_shot': includeOneShot,
        'include_automated': includeAutomated,
        'no_default_range': noDefaultRange,
        'breakdowns': breakdowns,
        'session_counts': sessionCounts,
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
   * Get usage pairwise comparison
   * @returns ServiceUsagePairwiseComparisonResponse OK
   * @throws ApiError
   */
  public static getApiV1UsagePairwiseComparison({
    leftDimension,
    leftValue,
    rightDimension,
    rightValue,
    from,
    to,
    timezone,
    agent,
    project,
    machine,
    gitBranch,
    excludeGitBranch,
    excludeProject,
    excludeProjectKey,
    excludeAgent,
    excludeModel,
    model,
    minUserMessages,
    activeSince,
    termination,
    includeOneShot = true,
    includeAutomated,
    noDefaultRange,
    breakdowns = true,
    sessionCounts = true,
  }: {
    /**
     * Left-side comparison dimension
     */
    leftDimension: string,
    /**
     * Left-side comparison value; opaque project key for the project dimension
     */
    leftValue: string,
    /**
     * Right-side comparison dimension
     */
    rightDimension: string,
    /**
     * Right-side comparison value; opaque project key for the project dimension
     */
    rightValue: string,
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
     */
    gitBranch?: string,
    /**
     * Exclude a git branch; opaque (project, branch) tokens from the /branches endpoint
     */
    excludeGitBranch?: string,
    /**
     * Exclude a project
     */
    excludeProject?: string,
    /**
     * Exclude an opaque project key
     */
    excludeProjectKey?: string,
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
     * Filter by termination status
     */
    termination?: string,
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
    /**
     * Preserve omitted from/to without applying default range
     */
    noDefaultRange?: boolean,
    /**
     * Include per-model, per-project, and per-agent breakdowns
     */
    breakdowns?: boolean,
    /**
     * Include distinct session counts
     */
    sessionCounts?: boolean,
  }): CancelablePromise<ServiceUsagePairwiseComparisonResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/usage/pairwise-comparison',
      query: {
        'from': from,
        'to': to,
        'timezone': timezone,
        'agent': agent,
        'project': project,
        'machine': machine,
        'git_branch': gitBranch,
        'exclude_git_branch': excludeGitBranch,
        'exclude_project': excludeProject,
        'exclude_project_key': excludeProjectKey,
        'exclude_agent': excludeAgent,
        'exclude_model': excludeModel,
        'model': model,
        'min_user_messages': minUserMessages,
        'active_since': activeSince,
        'termination': termination,
        'include_one_shot': includeOneShot,
        'include_automated': includeAutomated,
        'no_default_range': noDefaultRange,
        'breakdowns': breakdowns,
        'session_counts': sessionCounts,
        'left_dimension': leftDimension,
        'left_value': leftValue,
        'right_dimension': rightDimension,
        'right_value': rightValue,
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
    gitBranch,
    excludeGitBranch,
    excludeProject,
    excludeProjectKey,
    excludeAgent,
    excludeModel,
    model,
    minUserMessages,
    activeSince,
    termination,
    includeOneShot = true,
    includeAutomated,
    noDefaultRange,
    breakdowns = true,
    sessionCounts = true,
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
     */
    gitBranch?: string,
    /**
     * Exclude a git branch; opaque (project, branch) tokens from the /branches endpoint
     */
    excludeGitBranch?: string,
    /**
     * Exclude a project
     */
    excludeProject?: string,
    /**
     * Exclude an opaque project key
     */
    excludeProjectKey?: string,
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
     * Filter by termination status
     */
    termination?: string,
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
    /**
     * Preserve omitted from/to without applying default range
     */
    noDefaultRange?: boolean,
    /**
     * Include per-model, per-project, and per-agent breakdowns
     */
    breakdowns?: boolean,
    /**
     * Include distinct session counts
     */
    sessionCounts?: boolean,
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
        'git_branch': gitBranch,
        'exclude_git_branch': excludeGitBranch,
        'exclude_project': excludeProject,
        'exclude_project_key': excludeProjectKey,
        'exclude_agent': excludeAgent,
        'exclude_model': excludeModel,
        'model': model,
        'min_user_messages': minUserMessages,
        'active_since': activeSince,
        'termination': termination,
        'include_one_shot': includeOneShot,
        'include_automated': includeAutomated,
        'no_default_range': noDefaultRange,
        'breakdowns': breakdowns,
        'session_counts': sessionCounts,
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
    gitBranch,
    excludeGitBranch,
    excludeProject,
    excludeProjectKey,
    excludeAgent,
    excludeModel,
    model,
    minUserMessages,
    activeSince,
    termination,
    includeOneShot = true,
    includeAutomated,
    noDefaultRange,
    breakdowns = true,
    sessionCounts = true,
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
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
     */
    gitBranch?: string,
    /**
     * Exclude a git branch; opaque (project, branch) tokens from the /branches endpoint
     */
    excludeGitBranch?: string,
    /**
     * Exclude a project
     */
    excludeProject?: string,
    /**
     * Exclude an opaque project key
     */
    excludeProjectKey?: string,
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
     * Filter by termination status
     */
    termination?: string,
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
    /**
     * Preserve omitted from/to without applying default range
     */
    noDefaultRange?: boolean,
    /**
     * Include per-model, per-project, and per-agent breakdowns
     */
    breakdowns?: boolean,
    /**
     * Include distinct session counts
     */
    sessionCounts?: boolean,
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
        'git_branch': gitBranch,
        'exclude_git_branch': excludeGitBranch,
        'exclude_project': excludeProject,
        'exclude_project_key': excludeProjectKey,
        'exclude_agent': excludeAgent,
        'exclude_model': excludeModel,
        'model': model,
        'min_user_messages': minUserMessages,
        'active_since': activeSince,
        'termination': termination,
        'include_one_shot': includeOneShot,
        'include_automated': includeAutomated,
        'no_default_range': noDefaultRange,
        'breakdowns': breakdowns,
        'session_counts': sessionCounts,
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
