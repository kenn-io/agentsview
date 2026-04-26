import type { SessionTiming } from "./types/timing.js";
import { ApiError, getAuthToken, getBase } from "./client.js";

/** Fetch the per-session timing summary computed by the backend.
 *  Mirrors the Go GetSessionTiming handler at
 *  GET /api/v1/sessions/{id}/timing. */
export async function fetchSessionTiming(
  sessionId: string,
): Promise<SessionTiming> {
  const headers = new Headers();
  const token = getAuthToken();
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }
  const res = await fetch(
    `${getBase()}/sessions/${encodeURIComponent(sessionId)}/timing`,
    { headers },
  );
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(
      res.status,
      body.trim() || `session timing ${res.status}`,
    );
  }
  return (await res.json()) as SessionTiming;
}
