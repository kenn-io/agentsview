/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { AgentsResponse } from '../models/AgentsResponse';
import type { BranchesResponse } from '../models/BranchesResponse';
import type { DbSessionStats } from '../models/DbSessionStats';
import type { DbStats } from '../models/DbStats';
import type { MachinesResponse } from '../models/MachinesResponse';
import type { ProjectsResponse } from '../models/ProjectsResponse';
import type { UpdateCheckResponse } from '../models/UpdateCheckResponse';
import type { VersionInfo } from '../models/VersionInfo';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class MetadataService {
  /**
   * List agents
   * @returns AgentsResponse OK
   * @throws ApiError
   */
  public static getApiV1Agents({
    includeOneShot,
    includeAutomated,
  }: {
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
  }): CancelablePromise<AgentsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/agents',
      query: {
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
   * List branches
   * @returns BranchesResponse OK
   * @throws ApiError
   */
  public static getApiV1Branches({
    includeOneShot,
    includeAutomated,
  }: {
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
  }): CancelablePromise<BranchesResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/branches',
      query: {
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
   * List machines
   * @returns MachinesResponse OK
   * @throws ApiError
   */
  public static getApiV1Machines({
    includeOneShot,
    includeAutomated,
  }: {
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
  }): CancelablePromise<MachinesResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/machines',
      query: {
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
   * List projects
   * @returns ProjectsResponse OK
   * @throws ApiError
   */
  public static getApiV1Projects({
    includeOneShot,
    includeAutomated,
  }: {
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
  }): CancelablePromise<ProjectsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/projects',
      query: {
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
   * Get session stats
   * @returns DbSessionStats OK
   * @throws ApiError
   */
  public static getApiV1SessionStats({
    includeOneShot,
    includeAutomated,
    since,
    until,
    agent,
    includeProject,
    excludeProject,
    timezone,
    includeGitOutcomes,
    includeGithubOutcomes,
  }: {
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
    /**
     * Start of window
     */
    since?: string,
    /**
     * End of window
     */
    until?: string,
    /**
     * Filter by agent
     */
    agent?: string,
    /**
     * Restrict to these projects
     */
    includeProject?: any[] | null,
    /**
     * Exclude these projects
     */
    excludeProject?: any[] | null,
    /**
     * IANA timezone name
     */
    timezone?: string,
    /**
     * Include git-derived outcome stats
     */
    includeGitOutcomes?: boolean,
    /**
     * Include GitHub PR outcome stats
     */
    includeGithubOutcomes?: boolean,
  }): CancelablePromise<DbSessionStats> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/session-stats',
      query: {
        'include_one_shot': includeOneShot,
        'include_automated': includeAutomated,
        'since': since,
        'until': until,
        'agent': agent,
        'include_project': includeProject,
        'exclude_project': excludeProject,
        'timezone': timezone,
        'include_git_outcomes': includeGitOutcomes,
        'include_github_outcomes': includeGithubOutcomes,
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
   * Get stats
   * @returns DbStats OK
   * @throws ApiError
   */
  public static getApiV1Stats({
    includeOneShot,
    includeAutomated,
  }: {
    /**
     * Include one-shot sessions
     */
    includeOneShot?: boolean,
    /**
     * Include automated sessions
     */
    includeAutomated?: boolean,
  }): CancelablePromise<DbStats> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/stats',
      query: {
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
   * Check for updates
   * @returns UpdateCheckResponse OK
   * @throws ApiError
   */
  public static getApiV1UpdateCheck(): CancelablePromise<UpdateCheckResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/update/check',
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
   * Get server version
   * @returns VersionInfo OK
   * @throws ApiError
   */
  public static getApiV1Version(): CancelablePromise<VersionInfo> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/version',
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
