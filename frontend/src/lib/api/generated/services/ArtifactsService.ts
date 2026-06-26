/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { ArtifactOriginsResponse } from '../models/ArtifactOriginsResponse';
import type { ArtifactPeersResponse } from '../models/ArtifactPeersResponse';
import type { ArtifactPostResponse } from '../models/ArtifactPostResponse';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class ArtifactsService {
  /**
   * List artifact origins
   * @returns ArtifactOriginsResponse OK
   * @throws ApiError
   */
  public static getApiV1ArtifactsOrigins(): CancelablePromise<ArtifactOriginsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/artifacts/origins',
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
   * List artifact peers
   * @returns ArtifactPeersResponse OK
   * @throws ApiError
   */
  public static getApiV1ArtifactsPeers(): CancelablePromise<ArtifactPeersResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/artifacts/peers',
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
   * Get latest artifact checkpoint
   * @returns string OK
   * @throws ApiError
   */
  public static getApiV1ArtifactsOriginCheckpoint({
    origin,
  }: {
    /**
     * Artifact origin ID
     */
    origin: string,
  }): CancelablePromise<string> {
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
   * @returns string OK
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
  }): CancelablePromise<string> {
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
