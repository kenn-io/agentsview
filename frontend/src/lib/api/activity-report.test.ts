// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  fetchActivityReport,
  fetchActivitySessions,
} from "./activity-report.js";

function streamResponse(frames: string[]): Response {
  const encoder = new TextEncoder();
  return new Response(
    new ReadableStream({
      start(controller) {
        for (const frame of frames) controller.enqueue(encoder.encode(frame));
        controller.close();
      },
    }),
    { headers: { "Content-Type": "text/event-stream" } },
  );
}

describe("activity report API", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  it("streams progress before returning the terminal report", async () => {
    const fetchMock = vi.fn().mockResolvedValue(streamResponse([
      'event: progress\ndata: {"phase":"loading_sessions","processed":12}\n\n',
      'event: report\ndata: {"report_id":"signed","sessions_total":301}\n\n',
    ]));
    vi.stubGlobal("fetch", fetchMock);
    const progress: unknown[] = [];

    const report = await fetchActivityReport(
      { preset: "month", date: "2026-07-01", timezone: "UTC", automation: "all" },
      undefined,
      (value) => progress.push(value),
    );

    expect(progress).toEqual([
      { phase: "loading_sessions", processed: 12 },
    ]);
    expect(report).toMatchObject({ report_id: "signed", sessions_total: 301 });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(String(url)).toContain("/api/v1/activity/report?preset=month");
    expect((init.headers as Headers).get("Accept")).toBe(
      "text/event-stream, application/json",
    );
  });

  it("requests a deterministic server page with bucket and sort state", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      report_id: "signed",
      sessions: [{ session_id: "s2" }],
      next_cursor: "next",
      total: 8,
    }), { status: 200, headers: { "Content-Type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);

    const page = await fetchActivitySessions("signed", {
      limit: 200,
      cursor: "cursor",
      sort: "cost",
      direction: "asc",
      bucket: 7,
    });

    expect(page).toMatchObject({ next_cursor: "next", total: 8 });
    const requested = new URL(String(fetchMock.mock.calls[0]![0]), window.location.href);
    expect(requested.pathname).toBe("/api/v1/activity/report/signed/sessions");
    expect(Object.fromEntries(requested.searchParams)).toEqual({
      limit: "200",
      cursor: "cursor",
      sort: "cost",
      direction: "asc",
      bucket: "7",
    });
  });
});
