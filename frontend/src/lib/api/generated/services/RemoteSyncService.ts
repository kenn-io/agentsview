/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { RemotesyncTargetSet } from '../models/RemotesyncTargetSet';
import type { CancelablePromise } from '../core/CancelablePromise';
import { OpenAPI } from '../core/OpenAPI';
import { request as __request } from '../core/request';
export class RemoteSyncService {
  /**
   * Resolve remote sync targets
   * @returns RemotesyncTargetSet OK
   * @throws ApiError
   */
  public static getApiV1RemoteSyncTargets(): CancelablePromise<RemotesyncTargetSet> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/remote-sync/targets',
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
