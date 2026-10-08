import { reportTelemetry } from "./telemetry.js";

export function setupSessionEndedReporting(): () => void {
  let visibleMs = 0;
  let started = document.hidden ? undefined : performance.now();
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
      const bucket =
        visibleMs < 60_000
          ? "under_1m"
          : visibleMs < 300_000
            ? "1_to_5m"
            : visibleMs <= 1_800_000
              ? "5_to_30m"
              : "over_30m";
      reportTelemetry("session_ended", { surface: "web", duration_bucket: bucket });
    }
    visibleMs = 0;
  };
  const resume = () => {
    if (document.hidden) return;
    if (hiddenAt !== undefined && Date.now() - hiddenAt >= 1_800_000) end();
    clearTimeout(hiddenTimer);
    hiddenAt = undefined;
    if (started === undefined) started = performance.now();
  };
  const visibility = () => {
    if (document.hidden) {
      pause();
      hiddenAt = Date.now();
      hiddenTimer = setTimeout(end, 1_800_000);
    } else {
      resume();
    }
  };
  document.addEventListener("visibilitychange", visibility);
  window.addEventListener("pagehide", end);
  window.addEventListener("pageshow", resume);
  return () => {
    clearTimeout(hiddenTimer);
    document.removeEventListener("visibilitychange", visibility);
    window.removeEventListener("pagehide", end);
    window.removeEventListener("pageshow", resume);
  };
}
