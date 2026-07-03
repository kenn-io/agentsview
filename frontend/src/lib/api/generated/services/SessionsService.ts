/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { BatchDeleteInputBody } from '../models/BatchDeleteInputBody';
import type { DbSession } from '../models/DbSession';
import type { DbSessionActivityResponse } from '../models/DbSessionActivityResponse';
import type { DbSessionTiming } from '../models/DbSessionTiming';
import type { DbSidebarSessionIndex } from '../models/DbSidebarSessionIndex';
import type { EmptyTrashResponse } from '../models/EmptyTrashResponse';
import type { OpenRequest } from '../models/OpenRequest';
import type { OpenSessionResponse } from '../models/OpenSessionResponse';
import type { OrdinalsResponse } from '../models/OrdinalsResponse';
import type { PublishResponse } from '../models/PublishResponse';
import type { RenameRequest } from '../models/RenameRequest';
import type { ResolveSessionIDsResponse } from '../models/ResolveSessionIDsResponse';
import type { ResumeRequest } from '../models/ResumeRequest';
import type { ResumeResponse } from '../models/ResumeResponse';
import type { ServiceMessageList } from '../models/ServiceMessageList';
import type { ServiceSessionDetail } from '../models/ServiceSessionDetail';
import type { ServiceSessionList } from '../models/ServiceSessionList';
import type { ServiceToolCallList } from '../models/ServiceToolCallList';
import type { SessionDirectoryResponse } from '../models/SessionDirectoryResponse';
import type { SessionUsageResponse } from '../models/SessionUsageResponse';
import type { TrashResponse } from '../models/TrashResponse';
import type { UploadSessionResponse } from '../models/UploadSessionResponse';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class SessionsService {
  /**
   * Watch server events
   * @returns string OK
   * @throws ApiError
   */
  public static getApiV1Events(): CancelablePromise<string> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/events',
      errors: {
        400: `Bad Request`,
        401: `Unauthorized`,
        403: `Forbidden`,
        404: `Not Found`,
        409: `Conflict`,
        500: `Internal Server Error`,
        501: `Not Implemented`,
        502: `Bad Gateway`,
        503: `Service Unavailable`,
        504: `Gateway Timeout`,
      },
    });
  }
  /**
   * Resolve session IDs
   * @returns ResolveSessionIDsResponse OK
   * @throws ApiError
   */
  public static getApiV1SessionIdsResolve({
    partial,
    limit,
  }: {
    /**
     * Session ID substring
     */
    partial: string,
    /**
     * Maximum number of matching IDs
     */
    limit?: number,
  }): CancelablePromise<ResolveSessionIDsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/session-ids/resolve',
      query: {
        'partial': partial,
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
   * List sessions
   * @returns ServiceSessionList OK
   * @throws ApiError
   */
  public static getApiV1Sessions({
    project,
    excludeProject,
    machine,
    gitBranch,
    agent,
    date,
    dateFrom,
    dateTo,
    activeSince,
    minMessages,
    maxMessages,
    minUserMessages,
    includeOneShot,
    includeAutomated,
    includeChildren,
    outcome,
    healthGrade,
    cursor,
    limit,
    termination,
    minToolFailures,
    hasSecret,
    starred,
    orderBy = 'recent',
    descending,
  }: {
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
     * Minimum total message count
     */
    minMessages?: number,
    /**
     * Maximum total message count
     */
    maxMessages?: number,
    /**
     * Minimum user message count
     */
    minUserMessages?: number,
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
    /**
     * Include child sessions
     */
    includeChildren?: boolean,
    /**
     * Filter by detected outcome
     */
    outcome?: string,
    /**
     * Filter by health grade
     */
    healthGrade?: string,
    /**
     * Opaque pagination cursor
     */
    cursor?: string,
    /**
     * Maximum number of results
     */
    limit?: number,
    /**
     * Filter by termination reason
     */
    termination?: string,
    /**
     * Minimum tool failure count
     */
    minToolFailures?: number,
    /**
     * Filter sessions with secret findings
     */
    hasSecret?: boolean,
    /**
     * Filter sessions by starred status
     */
    starred?: boolean,
    /**
     * Sort order: a comma-separated list of keys, each optionally suffixed :asc or :desc (e.g. messages:desc,started:asc). A key with no suffix uses the descending param, then its natural direction. Valid keys: recent, started, messages, user-messages, output-tokens, peak-context, failures, retries, edit-churn, compactions, context-pressure, health, secrets, id.
     */
    orderBy?: string,
    /**
     * Default sort direction for keys in order_by that carry no explicit :asc/:desc suffix
     */
    descending?: boolean,
  }): CancelablePromise<ServiceSessionList> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sessions',
      query: {
        'project': project,
        'exclude_project': excludeProject,
        'machine': machine,
        'git_branch': gitBranch,
        'agent': agent,
        'date': date,
        'date_from': dateFrom,
        'date_to': dateTo,
        'active_since': activeSince,
        'min_messages': minMessages,
        'max_messages': maxMessages,
        'min_user_messages': minUserMessages,
        'include_one_shot': includeOneShot,
        'include_automated': includeAutomated,
        'include_children': includeChildren,
        'outcome': outcome,
        'health_grade': healthGrade,
        'cursor': cursor,
        'limit': limit,
        'termination': termination,
        'min_tool_failures': minToolFailures,
        'has_secret': hasSecret,
        'starred': starred,
        'order_by': orderBy,
        'descending': descending,
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
   * Batch delete sessions
   * @returns void
   * @throws ApiError
   */
  public static postApiV1SessionsBatchDelete({
    requestBody,
  }: {
    requestBody: BatchDeleteInputBody,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/sessions/batch-delete',
      body: requestBody,
      mediaType: 'application/json',
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
   * List sidebar sessions
   * @returns DbSidebarSessionIndex OK
   * @throws ApiError
   */
  public static getApiV1SessionsSidebarIndex({
    project,
    excludeProject,
    machine,
    gitBranch,
    agent,
    date,
    dateFrom,
    dateTo,
    activeSince,
    minMessages,
    maxMessages,
    minUserMessages,
    includeOneShot,
    includeAutomated,
    includeChildren,
    outcome,
    healthGrade,
    cursor,
    limit,
    termination,
    minToolFailures,
    hasSecret,
    starred,
    orderBy = 'recent',
    descending,
  }: {
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
     * Minimum total message count
     */
    minMessages?: number,
    /**
     * Maximum total message count
     */
    maxMessages?: number,
    /**
     * Minimum user message count
     */
    minUserMessages?: number,
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
    /**
     * Include child sessions
     */
    includeChildren?: boolean,
    /**
     * Filter by detected outcome
     */
    outcome?: string,
    /**
     * Filter by health grade
     */
    healthGrade?: string,
    /**
     * Opaque pagination cursor
     */
    cursor?: string,
    /**
     * Maximum number of results
     */
    limit?: number,
    /**
     * Filter by termination reason
     */
    termination?: string,
    /**
     * Minimum tool failure count
     */
    minToolFailures?: number,
    /**
     * Filter sessions with secret findings
     */
    hasSecret?: boolean,
    /**
     * Filter sessions by starred status
     */
    starred?: boolean,
    /**
     * Sort order: a comma-separated list of keys, each optionally suffixed :asc or :desc (e.g. messages:desc,started:asc). A key with no suffix uses the descending param, then its natural direction. Valid keys: recent, started, messages, user-messages, output-tokens, peak-context, failures, retries, edit-churn, compactions, context-pressure, health, secrets, id.
     */
    orderBy?: string,
    /**
     * Default sort direction for keys in order_by that carry no explicit :asc/:desc suffix
     */
    descending?: boolean,
  }): CancelablePromise<DbSidebarSessionIndex> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sessions/sidebar-index',
      query: {
        'project': project,
        'exclude_project': excludeProject,
        'machine': machine,
        'git_branch': gitBranch,
        'agent': agent,
        'date': date,
        'date_from': dateFrom,
        'date_to': dateTo,
        'active_since': activeSince,
        'min_messages': minMessages,
        'max_messages': maxMessages,
        'min_user_messages': minUserMessages,
        'include_one_shot': includeOneShot,
        'include_automated': includeAutomated,
        'include_children': includeChildren,
        'outcome': outcome,
        'health_grade': healthGrade,
        'cursor': cursor,
        'limit': limit,
        'termination': termination,
        'min_tool_failures': minToolFailures,
        'has_secret': hasSecret,
        'starred': starred,
        'order_by': orderBy,
        'descending': descending,
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
   * Upload a session export
   * @returns UploadSessionResponse OK
   * @throws ApiError
   */
  public static postApiV1SessionsUpload({
    project,
    machine = 'remote',
    formData,
  }: {
    /**
     * Project for imported session
     */
    project: string,
    /**
     * Machine name for imported session
     */
    machine?: string,
    formData?: {
      file: Blob;
    },
  }): CancelablePromise<UploadSessionResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/sessions/upload',
      query: {
        'project': project,
        'machine': machine,
      },
      formData: formData,
      mediaType: 'multipart/form-data',
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
   * Delete session
   * @returns void
   * @throws ApiError
   */
  public static deleteApiV1SessionsId({
    id,
  }: {
    /**
     * Session ID
     */
    id: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'DELETE',
      url: '/api/v1/sessions/{id}',
      path: {
        'id': id,
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
   * Get session
   * @returns ServiceSessionDetail OK
   * @throws ApiError
   */
  public static getApiV1SessionsId({
    id,
  }: {
    /**
     * Session ID
     */
    id: string,
  }): CancelablePromise<ServiceSessionDetail> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sessions/{id}',
      path: {
        'id': id,
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
   * Get session activity
   * @returns DbSessionActivityResponse OK
   * @throws ApiError
   */
  public static getApiV1SessionsIdActivity({
    id,
  }: {
    /**
     * Session ID
     */
    id: string,
  }): CancelablePromise<DbSessionActivityResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sessions/{id}/activity',
      path: {
        'id': id,
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
   * List child sessions
   * @returns any[] OK
   * @throws ApiError
   */
  public static getApiV1SessionsIdChildren({
    id,
  }: {
    /**
     * Session ID
     */
    id: string,
  }): CancelablePromise<any[] | null> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sessions/{id}/children',
      path: {
        'id': id,
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
   * Get session directory
   * @returns SessionDirectoryResponse OK
   * @throws ApiError
   */
  public static getApiV1SessionsIdDirectory({
    id,
  }: {
    /**
     * Session ID
     */
    id: string,
  }): CancelablePromise<SessionDirectoryResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sessions/{id}/directory',
      path: {
        'id': id,
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
   * Export session as HTML
   * @returns string OK
   * @throws ApiError
   */
  public static getApiV1SessionsIdExport({
    id,
  }: {
    /**
     * Session ID
     */
    id: string,
  }): CancelablePromise<string> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sessions/{id}/export',
      path: {
        'id': id,
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
   * Export session as Markdown
   * @returns string OK
   * @throws ApiError
   */
  public static getApiV1SessionsIdMd({
    id,
    depth,
  }: {
    /**
     * Session ID
     */
    id: string,
    /**
     * Child session depth
     */
    depth?: '1' | 'all',
  }): CancelablePromise<string> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sessions/{id}/md',
      path: {
        'id': id,
      },
      query: {
        'depth': depth,
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
   * List session messages
   * @returns ServiceMessageList OK
   * @throws ApiError
   */
  public static getApiV1SessionsIdMessages({
    id,
    limit,
    direction,
    from,
  }: {
    /**
     * Session ID
     */
    id: string,
    /**
     * Maximum number of messages
     */
    limit?: number,
    /**
     * Message ordering direction
     */
    direction?: 'asc' | 'desc',
    /**
     * Starting message ordinal
     */
    from?: number,
  }): CancelablePromise<ServiceMessageList> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sessions/{id}/messages',
      path: {
        'id': id,
      },
      query: {
        'limit': limit,
        'direction': direction,
        'from': from,
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
   * Open session directory
   * @returns OpenSessionResponse OK
   * @throws ApiError
   */
  public static postApiV1SessionsIdOpen({
    id,
    requestBody,
  }: {
    /**
     * Session ID
     */
    id: string,
    requestBody: OpenRequest,
  }): CancelablePromise<OpenSessionResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/sessions/{id}/open',
      path: {
        'id': id,
      },
      body: requestBody,
      mediaType: 'application/json',
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
   * Permanently delete session
   * @returns void
   * @throws ApiError
   */
  public static deleteApiV1SessionsIdPermanent({
    id,
  }: {
    /**
     * Session ID
     */
    id: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'DELETE',
      url: '/api/v1/sessions/{id}/permanent',
      path: {
        'id': id,
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
   * Publish session
   * @returns PublishResponse OK
   * @throws ApiError
   */
  public static postApiV1SessionsIdPublish({
    id,
    secret,
  }: {
    /**
     * Session ID
     */
    id: string,
    /**
     * Create a secret gist instead of a public one
     */
    secret?: boolean,
  }): CancelablePromise<PublishResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/sessions/{id}/publish',
      path: {
        'id': id,
      },
      query: {
        'secret': secret,
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
   * Rename session
   * @returns DbSession OK
   * @throws ApiError
   */
  public static patchApiV1SessionsIdRename({
    id,
    requestBody,
  }: {
    /**
     * Session ID
     */
    id: string,
    requestBody: RenameRequest,
  }): CancelablePromise<DbSession> {
    return __request(OpenAPI, {
      method: 'PATCH',
      url: '/api/v1/sessions/{id}/rename',
      path: {
        'id': id,
      },
      body: requestBody,
      mediaType: 'application/json',
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
   * Restore session
   * @returns void
   * @throws ApiError
   */
  public static postApiV1SessionsIdRestore({
    id,
  }: {
    /**
     * Session ID
     */
    id: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/sessions/{id}/restore',
      path: {
        'id': id,
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
   * Resume session
   * @returns ResumeResponse OK
   * @throws ApiError
   */
  public static postApiV1SessionsIdResume({
    id,
    requestBody,
  }: {
    /**
     * Session ID
     */
    id: string,
    requestBody: ResumeRequest,
  }): CancelablePromise<ResumeResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/sessions/{id}/resume',
      path: {
        'id': id,
      },
      body: requestBody,
      mediaType: 'application/json',
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
   * Search within a session
   * @returns OrdinalsResponse OK
   * @throws ApiError
   */
  public static getApiV1SessionsIdSearch({
    id,
    q,
  }: {
    /**
     * Session ID
     */
    id: string,
    /**
     * Search query
     */
    q?: string,
  }): CancelablePromise<OrdinalsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sessions/{id}/search',
      path: {
        'id': id,
      },
      query: {
        'q': q,
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
   * Get session timing
   * @returns DbSessionTiming OK
   * @throws ApiError
   */
  public static getApiV1SessionsIdTiming({
    id,
  }: {
    /**
     * Session ID
     */
    id: string,
  }): CancelablePromise<DbSessionTiming> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sessions/{id}/timing',
      path: {
        'id': id,
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
   * List session tool calls
   * @returns ServiceToolCallList OK
   * @throws ApiError
   */
  public static getApiV1SessionsIdToolCalls({
    id,
  }: {
    /**
     * Session ID
     */
    id: string,
  }): CancelablePromise<ServiceToolCallList> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sessions/{id}/tool-calls',
      path: {
        'id': id,
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
   * Get session usage
   * @returns SessionUsageResponse OK
   * @throws ApiError
   */
  public static getApiV1SessionsIdUsage({
    id,
  }: {
    /**
     * Session ID
     */
    id: string,
  }): CancelablePromise<SessionUsageResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sessions/{id}/usage',
      path: {
        'id': id,
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
   * Watch session events
   * @returns string OK
   * @throws ApiError
   */
  public static getApiV1SessionsIdWatch({
    id,
  }: {
    /**
     * Session ID
     */
    id: string,
  }): CancelablePromise<string> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sessions/{id}/watch',
      path: {
        'id': id,
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
   * Empty trash
   * @returns EmptyTrashResponse OK
   * @throws ApiError
   */
  public static deleteApiV1Trash(): CancelablePromise<EmptyTrashResponse> {
    return __request(OpenAPI, {
      method: 'DELETE',
      url: '/api/v1/trash',
      errors: {
        400: `Bad Request`,
        401: `Unauthorized`,
        403: `Forbidden`,
        404: `Not Found`,
        409: `Conflict`,
        500: `Internal Server Error`,
        501: `Not Implemented`,
        502: `Bad Gateway`,
        503: `Service Unavailable`,
        504: `Gateway Timeout`,
      },
    });
  }
  /**
   * List trash
   * @returns TrashResponse OK
   * @throws ApiError
   */
  public static getApiV1Trash(): CancelablePromise<TrashResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/trash',
      errors: {
        400: `Bad Request`,
        401: `Unauthorized`,
        403: `Forbidden`,
        404: `Not Found`,
        409: `Conflict`,
        500: `Internal Server Error`,
        501: `Not Implemented`,
        502: `Bad Gateway`,
        503: `Service Unavailable`,
        504: `Gateway Timeout`,
      },
    });
  }
}
