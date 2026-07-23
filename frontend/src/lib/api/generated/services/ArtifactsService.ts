/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { ArtifactFinalizeResponse } from '../models/ArtifactFinalizeResponse';
import type { ArtifactIndexResponse } from '../models/ArtifactIndexResponse';
import type { ArtifactOriginsResponse } from '../models/ArtifactOriginsResponse';
import type { ArtifactPeersResponse } from '../models/ArtifactPeersResponse';
import type { ArtifactPostResponse } from '../models/ArtifactPostResponse';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class ArtifactsService {
  /**
   * Finalize artifact uploads
   * @returns ArtifactFinalizeResponse OK
   * @throws ApiError
   */
  public static postApiV1ArtifactsFinalize(): CancelablePromise<ArtifactFinalizeResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/artifacts/finalize',
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
   * List artifact origins
   * @returns ArtifactOriginsResponse OK
   * @throws ApiError
   */
  public static getApiV1ArtifactsOrigins({
    cursor,
    limit = 512,
  }: {
    /**
     * Opaque artifact origin cursor
     */
    cursor?: string,
    /**
     * Maximum origins to return
     */
    limit?: number,
  }): CancelablePromise<ArtifactOriginsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/artifacts/origins',
      query: {
        'cursor': cursor,
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
   * List artifact peers
   * @returns ArtifactPeersResponse OK
   * @throws ApiError
   */
  public static getApiV1ArtifactsPeers({
    cursor,
    limit = 512,
  }: {
    /**
     * Opaque peer origin cursor
     */
    cursor?: string,
    /**
     * Maximum peer origins to return
     */
    limit?: number,
  }): CancelablePromise<ArtifactPeersResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/artifacts/peers',
      query: {
        'cursor': cursor,
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
   * Get latest artifact checkpoint
   * @returns binary OK
   * @throws ApiError
   */
  public static getApiV1ArtifactsOriginCheckpoint({
    origin,
  }: {
    /**
     * Artifact origin ID
     */
    origin: string,
  }): CancelablePromise<Blob> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/artifacts/{origin}/checkpoint',
      path: {
        'origin': origin,
      },
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
   * List artifact index for an origin
   * @returns ArtifactIndexResponse OK
   * @throws ApiError
   */
  public static getApiV1ArtifactsOriginIndex({
    origin,
    cursor,
    limit = 512,
  }: {
    /**
     * Artifact origin ID
     */
    origin: string,
    /**
     * Opaque artifact index cursor
     */
    cursor?: string,
    /**
     * Maximum artifact names to return
     */
    limit?: number,
  }): CancelablePromise<ArtifactIndexResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/artifacts/{origin}/index',
      path: {
        'origin': origin,
      },
      query: {
        'cursor': cursor,
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
   * Get artifact
   * @returns binary OK
   * @throws ApiError
   */
  public static getApiV1ArtifactsOriginKindName({
    origin,
    kind,
    name,
  }: {
    /**
     * Artifact origin ID
     */
    origin: string,
    /**
     * Artifact kind
     */
    kind: string,
    /**
     * Artifact filename or hash
     */
    name: string,
  }): CancelablePromise<Blob> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/artifacts/{origin}/{kind}/{name}',
      path: {
        'origin': origin,
        'kind': kind,
        'name': name,
      },
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
   * Post artifact
   * @returns ArtifactPostResponse OK
   * @throws ApiError
   */
  public static postApiV1ArtifactsOriginKindName({
    origin,
    kind,
    name,
    requestBody,
  }: {
    /**
     * Artifact origin ID
     */
    origin: string,
    /**
     * Artifact kind
     */
    kind: string,
    /**
     * Artifact filename or hash
     */
    name: string,
    requestBody: Blob,
  }): CancelablePromise<ArtifactPostResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/artifacts/{origin}/{kind}/{name}',
      path: {
        'origin': origin,
        'kind': kind,
        'name': name,
      },
      body: requestBody,
      mediaType: 'application/octet-stream',
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
