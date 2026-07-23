import type {
  RecallEntriesResponse,
  RecallEntry,
  RecallEntryFilters,
  RecallExtractionStatus,
} from "./types/recall.js";
import {
  ApiError,
  authHeaders,
  getBase,
  responseErrorMessage,
} from "./runtime.js";

const SESSION_RECALL_LIMIT = 500;
const RECALL_PAGE_LIMIT = 200;

export async function fetchRecallEntries(
  filters: RecallEntryFilters = {},
  signal?: AbortSignal,
): Promise<RecallEntry[]> {
  const query = new URLSearchParams({
    limit: String(filters.limit ?? RECALL_PAGE_LIMIT),
  });
  if (filters.query) query.set("q", filters.query);
  if (filters.project) query.set("project", filters.project);
  if (filters.type) query.set("type", filters.type);
  if (filters.sourceRunId) {
    query.set("source_run_id", filters.sourceRunId);
  }
  if (filters.reviewState) {
    query.set("review_state", filters.reviewState);
  }
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

export async function fetchRecallExtractionStatus(
  signal?: AbortSignal,
): Promise<RecallExtractionStatus> {
  const response = await fetch(
    `${getBase()}/recall/extraction/status`,
    authHeaders({ signal }),
  );
  if (!response.ok) {
    throw new ApiError(
      response.status,
      await responseErrorMessage(response),
    );
  }
  return (await response.json()) as RecallExtractionStatus;
}

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
