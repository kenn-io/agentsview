/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { EmbeddingsBuildRequest } from '../models/EmbeddingsBuildRequest';
import type { EmbeddingsBuildResponse } from '../models/EmbeddingsBuildResponse';
import type { EmbeddingsGenerationActionRequest } from '../models/EmbeddingsGenerationActionRequest';
import type { EmbeddingsGenerationsResponse } from '../models/EmbeddingsGenerationsResponse';
import type { VectorBuildStatus } from '../models/VectorBuildStatus';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class EmbeddingsService {
  /**
   * Start an embeddings build
   * @returns EmbeddingsBuildResponse OK
   * @throws ApiError
   */
  public static postApiV1EmbeddingsBuild({
    requestBody,
  }: {
    requestBody: EmbeddingsBuildRequest,
  }): CancelablePromise<EmbeddingsBuildResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/embeddings/build',
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
   * List embedding generations
   * @returns EmbeddingsGenerationsResponse OK
   * @throws ApiError
   */
  public static getApiV1EmbeddingsGenerations({
    store,
  }: {
    store?: string,
  }): CancelablePromise<EmbeddingsGenerationsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/embeddings/generations',
      query: {
        'store': store,
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
   * Activate an embedding generation
   * @returns void
   * @throws ApiError
   */
  public static postApiV1EmbeddingsGenerationsIdActivate({
    id,
    requestBody,
    store,
  }: {
    /**
     * Generation ordinal ID
     */
    id: number,
    requestBody: EmbeddingsGenerationActionRequest,
    store?: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/embeddings/generations/{id}/activate',
      path: {
        'id': id,
      },
      query: {
        'store': store,
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
   * Retire an embedding generation
   * @returns void
   * @throws ApiError
   */
  public static postApiV1EmbeddingsGenerationsIdRetire({
    id,
    requestBody,
    store,
  }: {
    /**
     * Generation ordinal ID
     */
    id: number,
    requestBody: EmbeddingsGenerationActionRequest,
    store?: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/embeddings/generations/{id}/retire',
      path: {
        'id': id,
      },
      query: {
        'store': store,
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
   * Embeddings build status
   * @returns VectorBuildStatus OK
   * @throws ApiError
   */
  public static getApiV1EmbeddingsStatus({
    store,
  }: {
    store?: string,
  }): CancelablePromise<VectorBuildStatus> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/embeddings/status',
      query: {
        'store': store,
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
