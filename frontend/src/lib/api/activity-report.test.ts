// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { fetchActivityReport, fetchActivitySessions } from "./activity-report.js";
import { setServerUrl } from "./runtime.js";

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
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        streamResponse([
          'event: progress\ndata: {"phase":"loading_sessions","processed":12}\n\n',
          'event: report\ndata: {"schema_version":9,"report_id":"signed","sessions_total":301}\n\n',
        ]),
      );
    vi.stubGlobal("fetch", fetchMock);
    const progress: unknown[] = [];

    const report = await fetchActivityReport(
      { preset: "month", date: "2026-07-01", timezone: "UTC", automation: "all" },
      undefined,
      (value) => progress.push(value),
    );

    expect(progress).toEqual([{ phase: "loading_sessions", processed: 12 }]);
    expect(report).toMatchObject({ report_id: "signed", sessions_total: 301 });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(String(url)).toContain("/api/v1/activity/report?preset=month");
    expect((init.headers as Headers).get("Accept")).toBe("text/event-stream, application/json");
  });

  it.each([
    { transport: "json", body: '{"schema_version":9,"report_id":"signed"}' },
    { transport: "json", body: '{"schema_version":10,"report_id":"signed"}' },
    { transport: "stream", body: '{"schema_version":10,"report_id":"signed"}' },
    { transport: "json", body: '{"report_id":"signed"}' },
  ])("passes through a $transport report", async ({ transport, body }) => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValue(
          transport === "stream"
            ? streamResponse([`event: report\ndata: ${body}\n\n`])
            : new Response(body, { headers: { "Content-Type": "application/json" } }),
        ),
    );
    const report = fetchActivityReport({ preset: "month" });
    await expect(report).resolves.toMatchObject(JSON.parse(body));
  });

  it.each([
    { version: 9 },
    { version: 10 },
  ])("passes through replacement report schema $version", async ({ version }) => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            report_id: "signed",
            sessions: [],
            total: 0,
            refresh_required: true,
            report: { schema_version: version },
          }),
          { headers: { "Content-Type": "application/json" } },
        ),
      ),
    );
    const page = fetchActivitySessions("signed", {});
    await expect(page).resolves.toMatchObject({ report: { schema_version: version } });
  });

  it("requests a deterministic server page with bucket range and sort state", async () => {
    setServerUrl("http://localhost");
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          report_id: "signed",
          sessions: [{ session_id: "s2" }],
          next_cursor: "next",
          total: 8,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const page = await fetchActivitySessions("signed", {
      limit: 200,
      cursor: "cursor",
      sort: "cost",
      direction: "asc",
      bucketRange: { start: 7, end: 9 },
    });

    expect(page).toMatchObject({ next_cursor: "next", total: 8 });
    const requested = new URL(String(fetchMock.mock.calls[0]![0]));
    expect(requested.pathname).toBe("/api/v1/activity/report/signed/sessions");
    expect(Object.fromEntries(requested.searchParams)).toEqual({
      limit: "200",
      cursor: "cursor",
      sort: "cost",
      direction: "asc",
      bucket_start: "7",
      bucket_end: "9",
    });
  });
});
