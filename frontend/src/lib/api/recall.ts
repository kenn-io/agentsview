import type {
  RecallEntry,
  RecallEntryFilters,
  RecallEntriesPage,
  RecallExtractProgressFilters,
  RecallExtractProgressPage,
  RecallExtractionStatus,
} from "./types/recall.js";
import { RecallService } from "./generated/index.js";

const SESSION_RECALL_LIMIT = 500;
const RECALL_PAGE_LIMIT = 200;
const RECALL_PROGRESS_LIMIT = 50;

export async function fetchRecallEntries(
  filters: RecallEntryFilters = {},
  signal?: AbortSignal,
): Promise<RecallEntriesPage> {
  const data = await RecallService.getApiV1RecallEntries(
    {
      limit: filters.limit ?? RECALL_PAGE_LIMIT,
      q: filters.query,
      project: filters.project,
      type: filters.type,
      source_run_id: filters.sourceRunId,
      review_state: filters.reviewState,
      cursor: filters.cursor,
    },
    { signal },
  );
  return {
    entries: data.entries ?? [],
    nextCursor: data.next_cursor || undefined,
    resultCap: data.result_cap || undefined,
  };
}

export async function fetchRecallExtractionStatus(
  signal?: AbortSignal,
): Promise<RecallExtractionStatus> {
  return RecallService.getApiV1RecallExtractionStatus({ signal });
}

export async function fetchRecallExtractionProgress(
  filters: RecallExtractProgressFilters = {},
  signal?: AbortSignal,
): Promise<RecallExtractProgressPage> {
  const data = await RecallService.getApiV1RecallExtractionProgress(
    {
      limit: filters.limit ?? RECALL_PROGRESS_LIMIT,
      generation: filters.generation,
      state: filters.state,
      cursor: filters.cursor,
    },
    { signal },
  );
  return {
    generationFingerprint: data.generation_fingerprint || undefined,
    progress: data.progress ?? [],
    nextCursor: data.next_cursor || undefined,
  };
}

export async function activateRecallExtractionGeneration(): Promise<void> {
  await RecallService.postApiV1RecallExtractionActivate();
}

export async function retireRecallExtractionGeneration(fingerprint: string): Promise<void> {
  await RecallService.postApiV1RecallExtractionGenerationsByFingerprintRetire({ fingerprint });
}

export async function fetchSessionRecall(
  sessionId: string,
  signal?: AbortSignal,
): Promise<RecallEntry[]> {
  const data = await RecallService.getApiV1RecallEntries(
    {
      source_session_id: sessionId,
      limit: SESSION_RECALL_LIMIT,
    },
    { signal },
  );
  return data.entries ?? [];
}
