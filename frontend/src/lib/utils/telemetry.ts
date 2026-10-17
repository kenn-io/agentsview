import { orvalRequest } from "../api/runtime.js";

export type TelemetryEvent =
  | "app_opened"
  | "visit_ended"
  | "screen_viewed"
  | "search_run"
  | "search_empty"
  | "error_shown"
  | "sync_failed"
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

export type ErrorKind = "search" | "session" | "insight" | "analytics" | "export";
export type SyncFailureReason =
  | "request"
  | "sign_in_required"
  | "browser_extension"
  | "already_running"
  | "other";

export function reportErrorShown(kind: ErrorKind): void {
  reportTelemetry("error_shown", { kind });
}

export function reportSyncFailed(error: unknown): void {
  let reason: SyncFailureReason = "other";
  const isError = error instanceof Error || error instanceof DOMException;
  const code =
    isError && "code" in error && typeof error.code === "string"
      ? error.code
      : undefined;
  switch (code) {
    case "sync_running":
    case "claude_ai_sync_running":
      reason = "already_running";
      break;
    case "claude_ai_auth_required":
      reason = "sign_in_required";
      break;
    case "claude_ai_chrome_host_required":
    case "claude_ai_chrome_host_update_required":
      reason = "browser_extension";
      break;
    default:
      if (isError && code === undefined) reason = "request";
  }

  reportTelemetry("sync_failed", { reason });
}
