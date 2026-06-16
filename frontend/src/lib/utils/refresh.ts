const MINUTE_MS = 60_000;
const HOUR_MS = 60 * MINUTE_MS;
const DAY_MS = 24 * HOUR_MS;

export function formatRefreshAge(
  updatedAt: number | null | undefined,
  now = Date.now(),
): string {
  if (updatedAt == null) return "Not updated";

  const ageMs = Math.max(0, now - updatedAt);
  if (ageMs < MINUTE_MS) return "Updated just now";
  if (ageMs < HOUR_MS) {
    return `Updated ${Math.floor(ageMs / MINUTE_MS)}m ago`;
  }
  if (ageMs < DAY_MS) {
    return `Updated ${Math.floor(ageMs / HOUR_MS)}h ago`;
  }
  return `Updated ${Math.floor(ageMs / DAY_MS)}d ago`;
}

export function createRefreshScheduler(
  refresh: () => void | Promise<void>,
  intervalMs: number,
) {
  let timer: ReturnType<typeof setTimeout> | undefined;

  function stop() {
    if (timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
    }
  }

  function runAndReschedule() {
    stop();
    void refresh();
    timer = setTimeout(runAndReschedule, intervalMs);
  }

  return {
    start: runAndReschedule,
    refreshNow: runAndReschedule,
    stop,
  };
}
