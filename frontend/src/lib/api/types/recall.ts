import type {
  DbRecallEvidence as RecallEvidence,
  DbRecallEntry as RecallEntry,
  RecallEntriesResponse,
  RecallExtractGenerationStatus as RecallExtractGeneration,
  RecallExtractionStatusResponse as RecallExtractionStatus,
  RecallExtractProgressItem as RecallExtractProgress,
  RecallExtractProgressItemState as RecallExtractProgressState,
  RecallExtractProgressResponse,
} from "../generated/index.js";
export type {
  RecallEvidence,
  RecallEntry,
  RecallEntriesResponse,
  RecallExtractGeneration,
  RecallExtractionStatus,
  RecallExtractProgress,
  RecallExtractProgressState,
  RecallExtractProgressResponse,
};

export interface RecallEntriesPage {
  entries: RecallEntry[];
  nextCursor?: string;
  resultCap?: number;
}

export type { DbExtractProgressStats as RecallExtractProgressStats } from "../generated/index.js";

export interface RecallExtractProgressPage {
  generationFingerprint?: string;
  progress: RecallExtractProgress[];
  nextCursor?: string;
}

export interface RecallExtractProgressFilters {
  generation?: string;
  state?: RecallExtractProgressState;
  limit?: number;
  cursor?: string;
}

export interface RecallEntryFilters {
  query?: string;
  project?: string;
  type?: string;
  sourceRunId?: string;
  reviewState?: string;
  limit?: number;
  cursor?: string;
}
