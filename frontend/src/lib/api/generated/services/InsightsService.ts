/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DbInsight } from '../models/DbInsight';
import type { GenerateInsightRequest } from '../models/GenerateInsightRequest';
import type { InsightsResponse } from '../models/InsightsResponse';
import type { PublishResponse } from '../models/PublishResponse';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class InsightsService {
  /**
   * List insights
   * @returns InsightsResponse OK
   * @throws ApiError
   */
  public static getApiV1Insights({
    type,
    project,
    dateFrom,
    dateTo,
  }: {
    /**
     * Insight type
     */
    type?: 'daily_activity' | 'agent_analysis' | 'llm_canned',
    /**
     * Filter by project
     */
    project?: string,
    /**
     * Filter date_from >= (YYYY-MM-DD)
     */
    dateFrom?: string,
    /**
     * Filter date_to <= (YYYY-MM-DD)
     */
    dateTo?: string,
  }): CancelablePromise<InsightsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/insights',
      query: {
        'type': type,
        'project': project,
        'date_from': dateFrom,
        'date_to': dateTo,
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
   * Generate insight
   * @returns string OK
   * @throws ApiError
   */
  public static postApiV1InsightsGenerate({
    requestBody,
  }: {
    requestBody: GenerateInsightRequest,
  }): CancelablePromise<string> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/insights/generate',
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
   * Delete insight
   * @returns void
   * @throws ApiError
   */
  public static deleteApiV1InsightsId({
    id,
  }: {
    /**
     * Numeric ID
     */
    id: number,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'DELETE',
      url: '/api/v1/insights/{id}',
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
   * Get insight
   * @returns DbInsight OK
   * @throws ApiError
   */
  public static getApiV1InsightsId({
    id,
  }: {
    /**
     * Numeric ID
     */
    id: number,
  }): CancelablePromise<DbInsight> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/insights/{id}',
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
   * Export insight as HTML
   * @returns string OK
   * @throws ApiError
   */
  public static getApiV1InsightsIdExport({
    id,
  }: {
    /**
     * Numeric ID
     */
    id: number,
  }): CancelablePromise<string> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/insights/{id}/export',
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
   * Export insight as Markdown
   * @returns string OK
   * @throws ApiError
   */
  public static getApiV1InsightsIdMd({
    id,
  }: {
    /**
     * Numeric ID
     */
    id: number,
  }): CancelablePromise<string> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/insights/{id}/md',
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
   * Publish insight
   * @returns PublishResponse OK
   * @throws ApiError
   */
  public static postApiV1InsightsIdPublish({
    id,
    secret,
  }: {
    /**
     * Insight ID
     */
    id: number,
    /**
     * Create a secret gist instead of a public one
     */
    secret?: boolean,
  }): CancelablePromise<PublishResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/insights/{id}/publish',
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
}
