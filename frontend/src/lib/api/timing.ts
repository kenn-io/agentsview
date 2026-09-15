import type { SessionTiming } from "./types/timing.js";
import { SessionsService } from "./generated/index.js";

/** Fetch the per-session timing summary computed by the backend. */
export function fetchSessionTiming(
  sessionId: string,
  signal?: AbortSignal,
): Promise<SessionTiming> {
  return SessionsService.getApiV1SessionsByIdTiming({ id: sessionId }, { signal });
}
