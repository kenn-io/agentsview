/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { RawsyncManifest } from '../models/RawsyncManifest';
import type { RawSyncManifestResponse } from '../models/RawSyncManifestResponse';
import type { RawSyncMissingObjectsInputBody } from '../models/RawSyncMissingObjectsInputBody';
import type { RawSyncMissingObjectsResponse } from '../models/RawSyncMissingObjectsResponse';
import type { RawSyncTokenInputBody } from '../models/RawSyncTokenInputBody';
import type { RawSyncTokenResponse } from '../models/RawSyncTokenResponse';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class RawSyncService {
  /**
   * Commit a raw manifest
   * @returns RawSyncManifestResponse OK
   * @throws ApiError
   */
  public static postApiV1RawSyncManifests({
    requestBody,
    authorization,
  }: {
    requestBody: RawsyncManifest,
    authorization?: string,
  }): CancelablePromise<RawSyncManifestResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/raw-sync/manifests',
      headers: {
        'Authorization': authorization,
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
   * Negotiate missing raw objects
   * @returns RawSyncMissingObjectsResponse OK
   * @throws ApiError
   */
  public static postApiV1RawSyncObjectsMissing({
    requestBody,
    authorization,
  }: {
    requestBody: RawSyncMissingObjectsInputBody,
    authorization?: string,
  }): CancelablePromise<RawSyncMissingObjectsResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/raw-sync/objects/missing',
      headers: {
        'Authorization': authorization,
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
   * Exchange a device credential
   * @returns RawSyncTokenResponse OK
   * @throws ApiError
   */
  public static postApiV1RawSyncTokens({
    requestBody,
    authorization,
    xAgentsViewDeviceId,
  }: {
    requestBody: RawSyncTokenInputBody,
    authorization?: string,
    xAgentsViewDeviceId?: string,
  }): CancelablePromise<RawSyncTokenResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/raw-sync/tokens',
      headers: {
        'Authorization': authorization,
        'X-AgentsView-Device-ID': xAgentsViewDeviceId,
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
}
