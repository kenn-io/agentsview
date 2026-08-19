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
import type { RawSyncUploadResponse } from '../models/RawSyncUploadResponse';
import type { RawSyncUploadStatusResponse } from '../models/RawSyncUploadStatusResponse';
import type { RawSyncUploadStartInputBody } from '../models/RawSyncUploadStartInputBody';
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
  /**
   * Create or resume a raw object upload
   * @returns RawSyncUploadResponse OK
   * @throws ApiError
   */
  public static postApiV1RawSyncUploads({
    requestBody,
    authorization,
  }: {
    requestBody: RawSyncUploadStartInputBody,
    authorization?: string,
  }): CancelablePromise<RawSyncUploadResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/api/v1/raw-sync/uploads',
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
   * Read a raw upload offset
   * @returns RawSyncUploadStatusResponse OK
   * @throws ApiError
   */
  public static headApiV1RawSyncUploadsUploadId({
    uploadId,
    authorization,
  }: {
    uploadId: string,
    authorization?: string,
  }): CancelablePromise<RawSyncUploadStatusResponse> {
    return __request(OpenAPI, {
      method: 'HEAD',
      url: '/api/v1/raw-sync/uploads/{upload_id}',
      path: {
        'upload_id': uploadId,
      },
      headers: {
        'Authorization': authorization,
      },
      responseHeaders: {
        offset: { name: 'Upload-Offset', type: 'number' },
        length: { name: 'Upload-Length', type: 'number' },
        complete: { name: 'Upload-Complete', type: 'boolean' },
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
   * Append a raw upload chunk
   * @returns RawSyncUploadResponse OK
   * @throws ApiError
   */
  public static patchApiV1RawSyncUploadsUploadId({
    uploadId,
    uploadOffset,
    requestBody,
    authorization,
  }: {
    uploadId: string,
    uploadOffset: number,
    requestBody: Blob,
    authorization?: string,
  }): CancelablePromise<RawSyncUploadResponse> {
    return __request(OpenAPI, {
      method: 'PATCH',
      url: '/api/v1/raw-sync/uploads/{upload_id}',
      path: {
        'upload_id': uploadId,
      },
      headers: {
        'Authorization': authorization,
        'Upload-Offset': uploadOffset,
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
