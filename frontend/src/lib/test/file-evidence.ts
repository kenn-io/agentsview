import type {
  ServiceLocalFileEvidence as Evidence,
  IntegrationsdocbankMatch as Match,
  ServiceFileReferenceOccurrence as Occurrence,
  ServiceFileReferencesResult as Result,
} from "../api/generated/index.js";
export const localEvidence = (overrides: Partial<Evidence> = {}): Evidence => ({
  occurrence_id: "o1",
  message_ordinal: 0,
  tool_call_index: null,
  result_event_index: null,
  block_index: 0,
  kind: "document",
  display_name: "report.txt",
  display_path: "",
  media_type: "text/plain",
  asset_ref: "",
  proof_method: "pi_native_write_utf8_v1",
  ineligible_reason: "",
  sha256: "a".repeat(64),
  size: 3,
  ...overrides,
});
export const fileOccurrence = (overrides: Partial<Occurrence> = {}): Occurrence => ({
  ...localEvidence(),
  state: "deferred",
  matches: [],
  checked_at: null,
  reference_total: null,
  next_reference_offset: null,
  ...overrides,
});
export const fileReferencesResult = (overrides: Partial<Result> = {}): Result => ({
  binding: "b1",
  revision: "r1",
  capability: "available",
  coverage: "complete",
  integration: "enabled",
  total: 1,
  occurrences: [fileOccurrence()],
  next_offset: null,
  ...overrides,
});
export const fileMatch = (overrides: Partial<Match> = {}): Match => ({
  node_id: 1,
  version_id: "v1",
  name: "first",
  state: "current",
  size: 3,
  sha256: "a".repeat(64),
  ...overrides,
});
export const imageEvidence = [
  {
    ordinal: 0,
    records: [
      localEvidence({ kind: "image", media_type: "image/png", asset_ref: "asset://proof.png" }),
    ],
  },
];
