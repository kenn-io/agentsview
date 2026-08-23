/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { RawsyncObjectRef } from './RawsyncObjectRef';
export type RawSyncUploadResponse = {
  complete: boolean;
  created: boolean;
  expires_at?: string;
  object: RawsyncObjectRef;
  offset: number;
  upload_id?: string;
};
