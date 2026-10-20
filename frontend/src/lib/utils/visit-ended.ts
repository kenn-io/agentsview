import type { BrowserTelemetryReporter } from "@kenn-io/kit-ui/utils/browser-telemetry";

const HIDDEN_VISIT_TIMEOUT_MS = 1_800_000;

function durationBucket(ms: number): string {
  if (ms < 60_000) return "under_1m";
  if (ms < 300_000) return "1_to_5m";
  if (ms < 1_800_000) return "5_to_30m";
  return "over_30m";
}

/** Reports visible visit time as a duration bucket when a visit ends; returns a cleanup. */
export function setupVisitEndedReporting(reporter: BrowserTelemetryReporter): () => void {
  let visibleMs = 0;
  let started: number | undefined;
  let tracking = false;
  let hiddenTimer: ReturnType<typeof setTimeout> | undefined;
  let hiddenAt: number | undefined;

  const pause = () => {
    if (started === undefined) return;
    visibleMs += performance.now() - started;
    started = undefined;
  };
  const end = () => {
    clearTimeout(hiddenTimer);
    hiddenAt = undefined;
    pause();
    if (visibleMs > 0) {
      reporter.reportVisit(
        {
          event: "visit_ended",
          properties: { surface: "web", duration_bucket: durationBucket(visibleMs) },
        },
        { keepalive: true },
      );
    }
    visibleMs = 0;
  };
  const resume = () => {
    if (document.hidden) return;
    if (hiddenAt !== undefined && Date.now() - hiddenAt >= HIDDEN_VISIT_TIMEOUT_MS) end();
    clearTimeout(hiddenTimer);
    hiddenAt = undefined;
    if (started === undefined) started = performance.now();
  };
  const visibility = () => {
    if (document.hidden) {
      pause();
      clearTimeout(hiddenTimer);
      hiddenAt = Date.now();
      hiddenTimer = setTimeout(end, HIDDEN_VISIT_TIMEOUT_MS);
    } else {
      resume();
    }
  };
  const stop = () => {
    tracking = false;
    clearTimeout(hiddenTimer);
    hiddenAt = undefined;
    started = undefined;
    visibleMs = 0;
    document.removeEventListener("visibilitychange", visibility);
    window.removeEventListener("pagehide", end);
    window.removeEventListener("pageshow", resume);
  };
  const unsubscribe = reporter.onQuietChange((quiet, reason) => {
    if (quiet && reason !== "checking") {
      stop();
    } else if (!tracking) {
      tracking = true;
      resume();
      document.addEventListener("visibilitychange", visibility);
      window.addEventListener("pagehide", end);
      window.addEventListener("pageshow", resume);
    }
  });
  return () => {
    unsubscribe();
    stop();
  };
}
