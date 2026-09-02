/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DataCandidatesResponse } from '../models/DataCandidatesResponse';
import type { DataCompactRequest } from '../models/DataCompactRequest';
import type { DataProjectRulesResponse } from '../models/DataProjectRulesResponse';
import type { DbCompactResult } from '../models/DbCompactResult';
import type { DbProjectInventory } from '../models/DbProjectInventory';
import type { DbSessionPage } from '../models/DbSessionPage';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class DataService {
  /**
   * Compact local archive
   * @returns DbCompactResult OK
   * @throws ApiError
   */
  public static postApiV1DataCompact({
    requestBody,
  }: {
    requestBody: DataCompactRequest,
  }): CancelablePromise<DbCompactResult> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/data/compact',
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
   * List archive-wide reclassification candidates
   * @returns DataCandidatesResponse OK
   * @throws ApiError
   */
  public static getApiV1DataProjectReclassificationCandidates({
    projectKey,
    projectLabel,
  }: {
    /**
     * Opaque project identity key
     */
    projectKey: string,
    /**
     * Project display label
     */
    projectLabel?: string,
  }): CancelablePromise<DataCandidatesResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/data/project-reclassification/candidates',
      query: {
        'project_label': projectLabel,
        'project_key': projectKey,
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
   * List project rules
   * @returns DataProjectRulesResponse OK
   * @throws ApiError
   */
  public static getApiV1DataProjectRules({
    machine,
  }: {
    /**
     * Machine to list rules for
     */
    machine?: string,
  }): CancelablePromise<DataProjectRulesResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/data/project-rules',
      query: {
        'machine': machine,
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
   * Get project inventory
   * @returns DbProjectInventory OK
   * @throws ApiError
   */
  public static getApiV1DataProjects(): CancelablePromise<DbProjectInventory> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/data/projects',
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
   * List sessions for an opaque project identity
   * @returns DbSessionPage OK
   * @throws ApiError
   */
  public static getApiV1DataProjectsProjectKeySessions({
    projectKey,
  }: {
    /**
     * Opaque project identity key
     */
    projectKey: string,
  }): CancelablePromise<DbSessionPage> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/data/projects/{project_key}/sessions',
      path: {
        'project_key': projectKey,
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
