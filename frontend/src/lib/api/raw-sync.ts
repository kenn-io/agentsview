import { orvalRequest } from "./runtime.js";

export interface RawSyncUploadStatus {
  offset: number;
  length: number;
  complete: boolean;
}

function requiredHeader(response: Response, name: string): string {
  const value = response.headers.get(name);
  if (value === null) throw new Error(`Response header ${name} is missing`);
  return value;
}

function numberHeader(response: Response, name: string): number {
  const value = Number(requiredHeader(response, name));
  if (!Number.isFinite(value)) {
    throw new Error(`Response header ${name} is not a number`);
  }
  return value;
}

export async function getRawSyncUploadStatus(
  uploadId: string,
  authorization: string,
  signal?: AbortSignal,
): Promise<RawSyncUploadStatus> {
  const response = await orvalRequest(`/api/v1/raw-sync/uploads/${uploadId}`, {
    headers: { Authorization: authorization },
    method: "HEAD",
    signal,
  });
  const complete = requiredHeader(response, "Upload-Complete");
  if (complete !== "true" && complete !== "false") {
    throw new Error("Response header Upload-Complete is not a boolean");
  }
  return {
    offset: numberHeader(response, "Upload-Offset"),
    length: numberHeader(response, "Upload-Length"),
    complete: complete === "true",
  };
}
