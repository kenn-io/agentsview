import {
  startBrowserTelemetry,
  type BrowserTelemetryReporter,
} from "@kenn-io/kit-ui/utils/browser-telemetry";
import {
  ApiError,
  getGeneratedBase,
  orvalRequest,
  SERVER_URL_CHANGE_EVENT,
  SERVER_URL_KEY,
} from "../api/runtime.js";
import { setupVisitEndedReporting } from "./visit-ended.js";

let reporter: BrowserTelemetryReporter | undefined;
let currentScreen: string | undefined;

/** Owns the selected server's gate and visit tracker for the mounted app. */
export function setupTelemetryReporting(): () => void {
  let stopVisit: (() => void) | undefined;
  const start = () => {
    if (reporter) return;
    const route = `${getGeneratedBase()}/api/v1/telemetry/events`;
    reporter = startBrowserTelemetry({
      route,
      surface: "web",
      storageKey: `agentsview.telemetry:${route}`,
      post: (_url, event, options) =>
        Promise.resolve().then(async () => {
          const controller = new AbortController();
          const timeout = setTimeout(() => controller.abort(), 10_000);
          try {
            const response = await orvalRequest("/api/v1/telemetry/events", {
              ...options,
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify(event),
              signal: controller.signal,
            });
            if (response.status !== 202) {
              throw new ApiError(response.status, `Unexpected telemetry HTTP ${response.status}`);
            }
            // Keep the deadline active while kit-ui reads the receipt body.
            return response;
          } catch (error) {
            clearTimeout(timeout);
            if (error instanceof ApiError && error.status === 401) {
              return { status: 401, data: undefined };
            }
            throw error;
          }
        }),
    });
    stopVisit = setupVisitEndedReporting(reporter);
    if (currentScreen !== undefined) reporter.reportScreen(currentScreen);
  };
  const stop = () => {
    stopVisit?.();
    reporter?.stop();
    reporter = undefined;
  };
  const replace = () => {
    stop();
    start();
  };
  const storage = (event: StorageEvent) => {
    if (event.storageArea === localStorage && (event.key === SERVER_URL_KEY || event.key === null))
      replace();
  };
  start();
  window.addEventListener(SERVER_URL_CHANGE_EVENT, replace);
  window.addEventListener("storage", storage);
  return () => {
    stop();
    currentScreen = undefined;
    window.removeEventListener(SERVER_URL_CHANGE_EVENT, replace);
    window.removeEventListener("storage", storage);
  };
}

export type TelemetryEvent =
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
export function reportTelemetry(event: TelemetryEvent, properties?: Record<string, string>): void {
  if (event === "screen_viewed") {
    currentScreen = properties?.screen;
    if (currentScreen !== undefined) reporter?.reportScreen(currentScreen);
  } else {
    // All non-screen outcomes use the gate so they wait for an enabled receipt.
    reporter?.reportVisit({ event, properties: properties ?? {} });
  }
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
