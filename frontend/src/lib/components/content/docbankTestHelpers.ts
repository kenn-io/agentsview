import { vi } from "vite-plus/test";
import { getApiV1SessionsByIdFileReferences } from "../../api/generated/sessions/sessions.js";
import type {
  ServiceFileReferenceOccurrence,
  ServiceFileReferencesResult,
} from "../../api/generated/index.js";
import { fileReferences } from "../../stores/file-references.svelte.js";

export const imageOccurrence: ServiceFileReferenceOccurrence = {
  message_ordinal: 0,
  tool_use_id: null,
  call_index: null,
  tool_name: null,
  event_index: null,
  image_index: 0,
  media_type: "image/png",
  size: 3,
  sha256: "a".repeat(64),
  state: "not_found",
  matches: [],
  match_total: 0,
};

export async function trackDocbank(
  result: Omit<ServiceFileReferencesResult, "transcript_revision">,
  id = "session-1",
) {
  fileReferences.enabled = true;
  await fileReferences.track(null, null);
  vi.mocked(getApiV1SessionsByIdFileReferences).mockResolvedValueOnce({
    ...result,
    transcript_revision: "revision-1",
  });
  await fileReferences.track(id, "revision-1");
}
