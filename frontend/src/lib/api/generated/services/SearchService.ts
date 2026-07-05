/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { SearchResponse } from '../models/SearchResponse';
import type { ServiceContentSearchResult } from '../models/ServiceContentSearchResult';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class SearchService {
  /**
   * Search sessions
   * @returns SearchResponse OK
   * @throws ApiError
   */
  public static getApiV1Search({
    q,
    project,
    sort = 'relevance',
    limit,
    cursor,
  }: {
    /**
     * Search query
     */
    q: string,
    /**
     * Filter by project
     */
    project?: string,
    /**
     * Sort order
     */
    sort?: 'relevance' | 'recency',
    /**
     * Maximum number of results
     */
    limit?: number,
    /**
     * Pagination cursor
     */
    cursor?: number,
  }): CancelablePromise<SearchResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/search',
      query: {
        'q': q,
        'project': project,
        'sort': sort,
        'limit': limit,
        'cursor': cursor,
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
   * Search session content
   * @returns ServiceContentSearchResult OK
   * @throws ApiError
   */
  public static getApiV1SearchContent({
    pattern,
    mode,
    _in,
    excludeSystem,
    reveal,
    project,
    excludeProject,
    machine,
    gitBranch,
    agent,
    date,
    dateFrom,
    dateTo,
    activeSince,
    includeChildren,
    includeAutomated,
    includeOneShot,
    limit,
    cursor,
    context,
  }: {
    /**
     * Pattern to search for
     */
    pattern: string,
    /**
     * Search mode
     */
    mode?: 'substring' | 'regex' | 'fts' | 'semantic' | 'hybrid',
    /**
     * Comma-separated content sources
     */
    _in?: string,
    /**
     * Exclude system messages
     */
    excludeSystem?: boolean,
    /**
     * Return unredacted secret matches for localhost callers
     */
    reveal?: boolean,
    /**
     * Filter by project
     */
    project?: string,
    /**
     * Exclude a project
     */
    excludeProject?: string,
    /**
     * Filter by machine
     */
    machine?: string,
    /**
     * Filter by git branch; opaque (project, branch) tokens from the /branches endpoint
     */
    gitBranch?: string,
    /**
     * Filter by agent
     */
    agent?: string,
    /**
     * Filter to a single YYYY-MM-DD date
     */
    date?: string,
    /**
     * Filter start date
     */
    dateFrom?: string,
    /**
     * Filter end date
     */
    dateTo?: string,
    /**
     * Filter sessions active since this RFC3339 timestamp
     */
    activeSince?: string,
    /**
     * Include child sessions
     */
    includeChildren?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Maximum number of results
     */
    limit?: number,
    /**
     * Pagination cursor
     */
    cursor?: number,
    /**
     * Include N messages of context before and after each match (max 10)
     */
    context?: number,
  }): CancelablePromise<ServiceContentSearchResult> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/search/content',
      query: {
        'pattern': pattern,
        'mode': mode,
        'in': _in,
        'exclude_system': excludeSystem,
        'reveal': reveal,
        'project': project,
        'exclude_project': excludeProject,
        'machine': machine,
        'git_branch': gitBranch,
        'agent': agent,
        'date': date,
        'date_from': dateFrom,
        'date_to': dateTo,
        'active_since': activeSince,
        'include_children': includeChildren,
        'include_automated': includeAutomated,
        'include_one_shot': includeOneShot,
        'limit': limit,
        'cursor': cursor,
        'context': context,
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
