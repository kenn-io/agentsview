import { describe, expect, it } from "vitest";
import { activeSessionsInSlot } from "./activeSessions.js";
import type {
  ActivityReportInterval,
  ActivitySessionRow,
} from "../../api/generated/index";
import { testMoney } from "../../test/money.js";

function row(id: string): ActivitySessionRow {
  return {
    session_id: id, title: id, project: "p", agent: "claude",
    primary_model: "m", models: ["m"], agent_minutes: 1, cost: testMoney(0),
    output_tokens: 0, first_active: null, last_active: null, timing_quality: "timed",
  } as ActivitySessionRow;
}
const ms = (iso: string) => Date.parse(iso);

describe("activeSessionsInSlot", () => {
  const bySession = new Map([row("a"), row("b")].map((r) => [r.session_id, r]));
  const slotStart = ms("2026-06-16T10:00:00Z");
  const slotEnd = ms("2026-06-16T10:05:00Z");

  it("dedups a session with multiple intervals in the slot to one row", () => {
    const intervals: ActivityReportInterval[] = [
      { session_id: "a", start: "2026-06-16T10:00:00Z", end: "2026-06-16T10:01:00Z" },
      { session_id: "a", start: "2026-06-16T10:01:00Z", end: "2026-06-16T10:02:00Z" },
      { session_id: "b", start: "2026-06-16T10:01:00Z", end: "2026-06-16T10:03:00Z" },
    ];
    const out = activeSessionsInSlot(intervals, slotStart, slotEnd, bySession);
    expect(out.map((r) => r.session_id)).toEqual(["a", "b"]); // a once, earliest-start first
  });

  it("excludes intervals that only touch the half-open boundaries", () => {
    const intervals: ActivityReportInterval[] = [
      { session_id: "a", start: "2026-06-16T09:55:00Z", end: "2026-06-16T10:00:00Z" }, // end == slotStart
      { session_id: "b", start: "2026-06-16T10:05:00Z", end: "2026-06-16T10:08:00Z" }, // start == slotEnd
    ];
    expect(activeSessionsInSlot(intervals, slotStart, slotEnd, bySession)).toEqual([]);
  });

  it("places a point interval (sub-second span) in the slot containing the instant", () => {
    // A sub-second span serialized at second resolution collapses to start==end.
    // The instant at the slot's start boundary belongs to the half-open slot...
    const atStart: ActivityReportInterval[] = [
      { session_id: "a", start: "2026-06-16T10:00:00Z", end: "2026-06-16T10:00:00Z" },
    ];
    expect(activeSessionsInSlot(atStart, slotStart, slotEnd, bySession).map((r) => r.session_id))
      .toEqual(["a"]);
    // ...while the instant exactly at the slot's end boundary belongs to the next slot.
    const atEnd: ActivityReportInterval[] = [
      { session_id: "a", start: "2026-06-16T10:05:00Z", end: "2026-06-16T10:05:00Z" },
    ];
    expect(activeSessionsInSlot(atEnd, slotStart, slotEnd, bySession)).toEqual([]);
  });

  it("skips ids missing from the session map", () => {
    const intervals: ActivityReportInterval[] = [
      { session_id: "ghost", start: "2026-06-16T10:01:00Z", end: "2026-06-16T10:02:00Z" },
    ];
    expect(activeSessionsInSlot(intervals, slotStart, slotEnd, bySession)).toEqual([]);
  });
});
