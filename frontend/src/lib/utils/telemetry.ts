import { orvalRequest, type ApiRequestOptions } from "../api/runtime.js";

export type TelemetryEvent =
  | "app_opened"
  | "session_ended"
  | "screen_viewed"
  | "search_run"
  | "session_viewed"
  | "export_run"
  | "insight_generated"
  | "analytics_viewed";

/** Posts a UI event to the daemon, which applies its allowlist; failures are ignored. */
export function reportTelemetry(
  event: TelemetryEvent,
  properties?: Record<string, string>,
  options?: ApiRequestOptions,
): void {
  const headers = new Headers(options?.headers);
  headers.set("Content-Type", "application/json");
  orvalRequest("/api/v1/telemetry/events", {
    ...options,
    method: "POST",
    headers,
    body: JSON.stringify({ event, properties }),
  }).catch(() => {});
}
