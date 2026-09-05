import type {
  RecallEntriesResponse,
  RecallEntry,
  RecallEntryFilters,
  RecallEntriesPage,
  RecallExtractProgressFilters,
  RecallExtractProgressPage,
  RecallExtractProgressResponse,
  RecallExtractionStatus,
} from "./types/recall.js";
import { ApiError, authHeaders, getBase, responseErrorMessage } from "./runtime.js";

const SESSION_RECALL_LIMIT = 500;
const RECALL_PAGE_LIMIT = 200;
const RECALL_PROGRESS_LIMIT = 50;

export async function fetchRecallEntries(
  filters: RecallEntryFilters = {},
  signal?: AbortSignal,
): Promise<RecallEntriesPage> {
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
  if (filters.cursor) query.set("cursor", filters.cursor);
  const response = await fetch(
    `${getBase()}/recall/entries?${query.toString()}`,
    authHeaders({ signal }),
  );
  if (!response.ok) {
    throw new ApiError(response.status, await responseErrorMessage(response));
  }
  const data = (await response.json()) as RecallEntriesResponse;
  return {
    entries: data.entries ?? [],
    nextCursor: data.next_cursor || undefined,
    resultCap: data.result_cap || undefined,
  };
}

export async function fetchRecallExtractionStatus(
  signal?: AbortSignal,
): Promise<RecallExtractionStatus> {
  const response = await fetch(`${getBase()}/recall/extraction/status`, authHeaders({ signal }));
  if (!response.ok) {
    throw new ApiError(response.status, await responseErrorMessage(response));
  }
  return (await response.json()) as RecallExtractionStatus;
}

export async function fetchRecallExtractionProgress(
  filters: RecallExtractProgressFilters = {},
  signal?: AbortSignal,
): Promise<RecallExtractProgressPage> {
  const query = new URLSearchParams({
    limit: String(filters.limit ?? RECALL_PROGRESS_LIMIT),
  });
  if (filters.generation) query.set("generation", filters.generation);
  if (filters.state) query.set("state", filters.state);
  if (filters.cursor) query.set("cursor", filters.cursor);
  const response = await fetch(
    `${getBase()}/recall/extraction/progress?${query.toString()}`,
    authHeaders({ signal }),
  );
  if (!response.ok) {
    throw new ApiError(response.status, await responseErrorMessage(response));
  }
  const data = (await response.json()) as RecallExtractProgressResponse;
  return {
    generationFingerprint: data.generation_fingerprint || undefined,
    progress: data.progress ?? [],
    nextCursor: data.next_cursor || undefined,
  };
}

async function postRecallExtractionAction(path: string): Promise<void> {
  const response = await fetch(`${getBase()}${path}`, authHeaders({ method: "POST" }));
  if (!response.ok) {
    throw new ApiError(response.status, await responseErrorMessage(response));
  }
}

export async function activateRecallExtractionGeneration(): Promise<void> {
  await postRecallExtractionAction("/recall/extraction/activate");
}

export async function retireRecallExtractionGeneration(fingerprint: string): Promise<void> {
  await postRecallExtractionAction(
    `/recall/extraction/generations/${encodeURIComponent(fingerprint)}/retire`,
  );
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
    throw new ApiError(response.status, await responseErrorMessage(response));
  }
  const data = (await response.json()) as RecallEntriesResponse;
  return data.entries ?? [];
}
