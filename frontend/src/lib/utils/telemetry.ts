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

const syncFailureReasons = new Map<string, SyncFailureReason>([
  ["sync_running", "already_running"],
  ["claude_ai_sync_running", "already_running"],
  ["claude_ai_auth_required", "sign_in_required"],
  ["claude_ai_chrome_host_required", "browser_extension"],
  ["claude_ai_chrome_host_update_required", "browser_extension"],
]);

export function reportSyncFailed(error: unknown): void {
  let reason: SyncFailureReason = "other";
  // DOMException does not extend Error in every runtime.
  if (error instanceof Error || error instanceof DOMException) {
    const code: unknown = "code" in error ? error.code : undefined;
    reason = typeof code === "string" ? (syncFailureReasons.get(code) ?? "other") : "request";
  }
  reportTelemetry("sync_failed", { reason });
}
