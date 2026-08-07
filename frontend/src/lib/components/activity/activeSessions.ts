import type { ReportInterval, SessionRow } from "../../api/types.js";

/**
 * The unique sessions whose interval overlaps the half-open slot
 * [slotStartMs, slotEndMs), joined to their session row, sorted by earliest
 * overlapping interval start then session id. A session with multiple intervals
 * in the slot appears once. Ids absent from `bySession` are skipped.
 */
export function activeSessionsInSlot(
  intervals: ReportInterval[],
  slotStartMs: number,
  slotEndMs: number,
  bySession: Map<string, SessionRow>,
): SessionRow[] {
  const earliestStart = new Map<string, number>();
  for (const iv of intervals) {
    const start = Date.parse(iv.start);
    const end = Date.parse(iv.end);
    // A sub-second active span is exposed at the report's second resolution (for
    // cross-backend parity) and so arrives as a point, start === end. Treat it
    // as the instant `start`, which belongs to the half-open slot containing it;
    // a positive interval uses the usual half-open overlap test.
    const overlaps =
      start === end
        ? start >= slotStartMs && start < slotEndMs
        : start < slotEndMs && end > slotStartMs;
    if (!overlaps) continue;
    const prev = earliestStart.get(iv.session_id);
    if (prev === undefined || start < prev) earliestStart.set(iv.session_id, start);
  }
  const ids = [...earliestStart.keys()].sort((a, b) => {
    const d = earliestStart.get(a)! - earliestStart.get(b)!;
    return d !== 0 ? d : a.localeCompare(b);
  });
  const rows: SessionRow[] = [];
  for (const id of ids) {
    const r = bySession.get(id);
    if (r) rows.push(r);
  }
  return rows;
}
