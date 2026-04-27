const DASH = "—";

export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return DASH;
  if (ms < 1_000) return `${Math.trunc(ms)}ms`;
  // Floor to one decimal so values just shy of a minute (e.g. 59_999ms)
  // can't round up to "60.0s" and read like a full minute. toFixed(1)
  // keeps the trailing zero for round seconds (e.g. 4_000 → "4.0s").
  if (ms < 60_000) return `${(Math.floor(ms / 100) / 10).toFixed(1)}s`;
  if (ms < 3_600_000) {
    const m = Math.floor(ms / 60_000);
    const s = Math.floor((ms % 60_000) / 1_000);
    return `${m}m ${s}s`;
  }
  const h = Math.floor(ms / 3_600_000);
  const m = Math.floor((ms % 3_600_000) / 60_000);
  return `${h}h ${m}m`;
}
