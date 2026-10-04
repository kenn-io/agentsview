import { reportTelemetry } from "./telemetry.js";

let lastSentDay = "";

function reportAppOpened(): void {
  const day = new Date().toISOString().slice(0, 10);
  if (day === lastSentDay) return;
  lastSentDay = day;
  reportTelemetry("app_opened");
}

/** Reports app_opened now and on the first window focus of each later UTC day; returns a cleanup. */
export function setupAppOpenedReporting(): () => void {
  reportAppOpened();
  window.addEventListener("focus", reportAppOpened);
  return () => window.removeEventListener("focus", reportAppOpened);
}
