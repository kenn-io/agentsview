import type { BrowserTelemetryReporter } from "@kenn-io/kit-ui/utils/browser-telemetry";

const HIDDEN_VISIT_TIMEOUT_MS = 1_800_000;

function durationBucket(ms: number): string {
  if (ms < 60_000) return "under_1m";
  if (ms < 300_000) return "1_to_5m";
  if (ms < 1_800_000) return "5_to_30m";
  return "over_30m";
}

/** Reports visible visit time as a duration bucket when a visit ends; returns a cleanup. */
export function setupVisitEndedReporting(
  reporter: BrowserTelemetryReporter,
  route: string,
): () => void {
  // Tab storage keeps ended visits through reloads while permission is pending.
  const pendingKey = `agentsview.telemetry.pending-visit:${route}`;
  let checking = true;
  let pending: string[] = [];
  try {
    const saved: unknown = JSON.parse(sessionStorage.getItem(pendingKey) ?? "[]");
    if (Array.isArray(saved)) {
      pending = saved
        .filter((bucket) => ["under_1m", "1_to_5m", "5_to_30m", "over_30m"].includes(bucket))
        .slice(0, 64);
    }
  } catch {
    // Visits remain optional when browser storage is blocked.
  }
  const savePending = () => {
    try {
      if (pending.length) sessionStorage.setItem(pendingKey, JSON.stringify(pending));
      else sessionStorage.removeItem(pendingKey);
    } catch {
      // The current page can still deliver pending visits.
    }
  };
  const report = (bucket: string) =>
    reporter.reportVisit(
      { event: "visit_ended", properties: { surface: "web", duration_bucket: bucket } },
      { keepalive: true },
    );
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
      const bucket = durationBucket(visibleMs);
      if (checking) {
        if (pending.length < 64) pending.push(bucket);
        savePending();
      } else report(bucket);
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
    checking = reason === "checking";
    if (!checking) {
      const ready = pending;
      pending = [];
      // Claim before posting so reloads cannot submit the same visit twice.
      savePending();
      if (!quiet) ready.forEach(report);
    }
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
    pending = [];
    savePending();
  };
}
