/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { RemoteSyncRequest } from '../models/RemoteSyncRequest';
import type { ServiceSessionDetail } from '../models/ServiceSessionDetail';
import type { ServiceSyncInput } from '../models/ServiceSyncInput';
import type { SyncStatusResponse } from '../models/SyncStatusResponse';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class SyncService {
  /**
   * Trigger full resync
   * @returns string OK
   * @throws ApiError
   */
  public static postApiV1Resync(): CancelablePromise<string> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/resync',
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
   * Sync a session
   * @returns ServiceSessionDetail OK
   * @throws ApiError
   */
  public static postApiV1SessionsSync({
    requestBody,
  }: {
    requestBody: ServiceSyncInput,
  }): CancelablePromise<ServiceSessionDetail> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/sessions/sync',
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
   * Trigger sync
   * @returns string OK
   * @throws ApiError
   */
  public static postApiV1Sync(): CancelablePromise<string> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/sync',
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
   * Sync remote hosts
   * @returns any OK
   * @throws ApiError
   */
  public static postApiV1SyncRemotes({
    requestBody,
  }: {
    requestBody: RemoteSyncRequest,
  }): CancelablePromise<Record<string, any>> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/sync/remotes',
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
   * Get sync status
   * @returns SyncStatusResponse OK
   * @throws ApiError
   */
  public static getApiV1SyncStatus(): CancelablePromise<SyncStatusResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/sync/status',
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
