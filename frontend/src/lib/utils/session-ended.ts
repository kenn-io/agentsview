import { getGeneratedBase } from "../api/runtime.js";
import { reportTelemetry } from "./telemetry.js";

const THIRTY_MINUTES = 1_800_000;

export function setupSessionEndedReporting(): () => void {
  let destination = getGeneratedBase();
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
    const currentDestination = getGeneratedBase();
    if (visibleMs > 0 && destination === currentDestination) {
      const bucket =
        visibleMs < 60_000
          ? "under_1m"
          : visibleMs < 300_000
            ? "1_to_5m"
            : visibleMs <= THIRTY_MINUTES
              ? "5_to_30m"
              : "over_30m";
      reportTelemetry(
        "session_ended",
        { surface: "web", duration_bucket: bucket },
        { keepalive: true, signal: AbortSignal.timeout(10_000) },
      );
    }
    visibleMs = 0;
    destination = currentDestination;
  };
  const resume = () => {
    if (document.hidden) return;
    if (hiddenAt !== undefined && Date.now() - hiddenAt >= THIRTY_MINUTES) end();
    clearTimeout(hiddenTimer);
    hiddenAt = undefined;
    if (started === undefined) started = performance.now();
  };
  const visibility = () => {
    if (document.hidden) {
      pause();
      hiddenAt = Date.now();
      hiddenTimer = setTimeout(end, THIRTY_MINUTES);
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
