import type {
  BrowserTelemetryQuietReason,
  BrowserTelemetryReporter,
} from "@kenn-io/kit-ui/utils/browser-telemetry";

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
  // Each ended visit has its own record, so closing a tab cannot erase it.
  const pendingPrefix = `agentsview.telemetry.pending-visit:${encodeURIComponent(route)}:`;
  const ownPending = new Set<string>();
  let permission: BrowserTelemetryQuietReason = "checking";
  let active = true;
  const pendingKeys = () =>
    Object.keys(localStorage).filter((key) => key.startsWith(pendingPrefix));
  const discardPending = () => {
    try {
      for (const key of pendingKeys()) localStorage.removeItem(key);
      ownPending.clear();
    } catch {
      // Browser storage can be blocked.
    }
  };
  const report = (bucket: string) =>
    reporter.reportVisit(
      { event: "visit_ended", properties: { surface: "web", duration_bucket: bucket } },
      { keepalive: true },
    );
  const replayPending = () => {
    // Web Locks serialize claims across tabs. Without them, leave records unsent.
    void navigator.locks
      ?.request(pendingPrefix, () => {
        if (!active || permission !== "enabled") return;
        for (const key of pendingKeys()) {
          const bucket = localStorage.getItem(key);
          // Claim before posting so another tab or reload cannot submit it twice.
          localStorage.removeItem(key);
          ownPending.delete(key);
          if (bucket && ["under_1m", "1_to_5m", "5_to_30m", "over_30m"].includes(bucket))
            report(bucket);
        }
      })
      .catch(() => {
        // A later enabled page can retry records it could not claim.
      });
  };
  const storage = (event: StorageEvent) => {
    if (event.storageArea !== localStorage || !event.key?.startsWith(pendingPrefix)) return;
    if (permission === "enabled") replayPending();
    else if (permission === "disabled") discardPending();
  };
  window.addEventListener("storage", storage);
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
      if (permission === "checking") {
        try {
          if (pendingKeys().length < 64) {
            const key = pendingPrefix + crypto.getRandomValues(new Uint32Array(4)).join("-");
            localStorage.setItem(key, bucket);
            ownPending.add(key);
          }
        } catch {
          // Visits remain optional when browser storage is blocked.
        }
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
    permission = reason;
    if (reason === "disabled") discardPending();
    else if (!quiet) replayPending();
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
    active = false;
    window.removeEventListener("storage", storage);
    try {
      for (const key of ownPending) localStorage.removeItem(key);
    } catch {
      // Cleanup must not interrupt a server change.
    }
  };
}
