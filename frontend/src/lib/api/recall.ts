import type {
  RecallEntriesResponse,
  RecallEntry,
} from "./types/recall.js";
import {
  ApiError,
  authHeaders,
  getBase,
  responseErrorMessage,
} from "./runtime.js";

const SESSION_RECALL_LIMIT = 500;

export async function fetchSessionRecall(
  sessionId: string,
  signal?: AbortSignal,
): Promise<RecallEntry[]> {
  const query = new URLSearchParams({
    source_session_id: sessionId,
    limit: String(SESSION_RECALL_LIMIT),
  });
  const response = await fetch(
    `${getBase()}/recall/entries?${query.toString()}`,
    authHeaders({ signal }),
  );
  if (!response.ok) {
    throw new ApiError(
      response.status,
      await responseErrorMessage(response),
    );
  }
  const data = (await response.json()) as RecallEntriesResponse;
  return data.entries ?? [];
}
