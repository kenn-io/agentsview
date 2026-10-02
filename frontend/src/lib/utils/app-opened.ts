import { orvalRequest } from "../api/runtime.js";

const ENDPOINT = "/api/v1/telemetry/events";
let lastSentDay = "";

function reportAppOpened(): void {
  const day = new Date().toISOString().slice(0, 10);
  if (day === lastSentDay) return;
  lastSentDay = day;
  orvalRequest(ENDPOINT, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ event: "app_opened" }),
  }).catch(() => {});
}

/** Reports app_opened now and on the first window focus of each later UTC day; returns a cleanup. */
export function setupAppOpenedReporting(): () => void {
  reportAppOpened();
  window.addEventListener("focus", reportAppOpened);
  return () => window.removeEventListener("focus", reportAppOpened);
}
