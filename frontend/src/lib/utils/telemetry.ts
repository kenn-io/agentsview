import { orvalRequest } from "../api/runtime.js";

export type TelemetryEvent =
  | "app_opened"
  | "visit_ended"
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
  options?: Pick<RequestInit, "keepalive" | "signal">,
): void {
  orvalRequest("/api/v1/telemetry/events", {
    ...options,
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ event, properties }),
  }).catch(() => {});
}
