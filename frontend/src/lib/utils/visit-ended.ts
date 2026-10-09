import {
  getGeneratedBase,
  SERVER_URL_CHANGE_EVENT,
  SERVER_URL_KEY,
  SERVER_URL_REVISION_KEY,
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
  let revision = localStorage.getItem(SERVER_URL_REVISION_KEY);
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
    if (
      visibleMs > 0 &&
      !invalid &&
      destination === getGeneratedBase() &&
      revision === localStorage.getItem(SERVER_URL_REVISION_KEY)
    ) {
      reportTelemetry(
        "visit_ended",
        { surface: "web", duration_bucket: durationBucket(visibleMs) },
        { keepalive: true },
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
        revision = localStorage.getItem(SERVER_URL_REVISION_KEY);
      }
      started = performance.now();
    }
  };
  const serverChanged = () => {
    // Queued storage events can predate this visit's shared revision.
    if (
      destination !== undefined &&
      (destination !== getGeneratedBase() ||
        revision !== localStorage.getItem(SERVER_URL_REVISION_KEY))
    ) {
      invalid = true;
    }
  };
  const storage = (event: StorageEvent) => {
    if (
      event.storageArea === localStorage &&
      (event.key === SERVER_URL_KEY || event.key === null)
    ) {
      serverChanged();
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
  window.addEventListener(SERVER_URL_CHANGE_EVENT, serverChanged);
  return () => {
    clearTimeout(hiddenTimer);
    document.removeEventListener("visibilitychange", visibility);
    window.removeEventListener("pagehide", end);
    window.removeEventListener("pageshow", resume);
    window.removeEventListener("storage", storage);
    window.removeEventListener(SERVER_URL_CHANGE_EVENT, serverChanged);
  };
}
