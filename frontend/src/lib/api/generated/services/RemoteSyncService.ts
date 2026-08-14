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
  public static getApiV1RemoteSyncTargets({
    xAgentsViewRemoteSyncVersion,
  }: {
    /**
     * Required remote-sync protocol version
     */
    xAgentsViewRemoteSyncVersion?: string,
  }): CancelablePromise<RemotesyncTargetSet> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/api/v1/remote-sync/targets',
      headers: {
        'X-AgentsView-Remote-Sync-Version': xAgentsViewRemoteSyncVersion,
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
