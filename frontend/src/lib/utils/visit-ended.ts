import {
  getGeneratedBase,
  getServerUrl,
  SERVER_URL_CHANGE_EVENT,
  SERVER_URL_KEY,
} from "../api/runtime.js";
import { reportTelemetry } from "./telemetry.js";

const HIDDEN_VISIT_TIMEOUT_MS = 1_800_000;

function durationBucket(ms: number): string {
  if (ms < 60_000) return "under_1m";
  if (ms < 300_000) return "1_to_5m";
  if (ms < 1_800_000) return "5_to_30m";
  return "over_30m";
}

/** Reports visible visit time as a duration bucket when a visit ends; returns a cleanup. */
export function setupVisitEndedReporting(): () => void {
  let visibleMs = 0;
  let started = document.hidden ? undefined : performance.now();
  let destination = started === undefined ? undefined : getGeneratedBase();
  let selection = getServerUrl();
  let invalid = false;
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
    if (visibleMs > 0 && !invalid && destination === getGeneratedBase()) {
      reportTelemetry(
        "visit_ended",
        { surface: "web", duration_bucket: durationBucket(visibleMs) },
        { keepalive: true, signal: AbortSignal.timeout(10_000) },
      );
    }
    visibleMs = 0;
    destination = undefined;
    invalid = false;
  };
  const resume = () => {
    if (document.hidden) return;
    if (hiddenAt !== undefined && Date.now() - hiddenAt >= HIDDEN_VISIT_TIMEOUT_MS) end();
    clearTimeout(hiddenTimer);
    hiddenAt = undefined;
    if (started === undefined) {
      if (destination === undefined) {
        destination = getGeneratedBase();
        selection = getServerUrl();
      }
      started = performance.now();
    }
  };
  const serverChanged = (server: string) => {
    if (destination !== undefined && server !== selection) invalid = true;
  };
  const localServerChange = () => serverChanged(getServerUrl());
  const storage = (event: StorageEvent) => {
    if (
      event.storageArea === localStorage &&
      (event.key === SERVER_URL_KEY || event.key === null)
    ) {
      serverChanged(event.newValue ?? "");
    }
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
  document.addEventListener("visibilitychange", visibility);
  window.addEventListener("pagehide", end);
  window.addEventListener("pageshow", resume);
  window.addEventListener("storage", storage);
  window.addEventListener(SERVER_URL_CHANGE_EVENT, localServerChange);
  return () => {
    clearTimeout(hiddenTimer);
    document.removeEventListener("visibilitychange", visibility);
    window.removeEventListener("pagehide", end);
    window.removeEventListener("pageshow", resume);
    window.removeEventListener("storage", storage);
    window.removeEventListener(SERVER_URL_CHANGE_EVENT, localServerChange);
  };
}
