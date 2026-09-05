import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { AnalyticsService } from "./generated/index.js";

describe("AnalyticsService signal sessions", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("includes the model filter in signal session requests", async () => {
    localStorage.setItem("agentsview-server-url", "http://localhost");
    const fetchMock = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) =>
        new Response(JSON.stringify({ sessions: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);
    await AnalyticsService.getApiV1AnalyticsSignalSessions({
      signal: "runaway_tool_loop_count",
      from: "2024-06-01",
      to: "2024-06-07",
      timezone: "UTC",
      model: "gpt-4o",
    });

    const url = new URL(String(fetchMock.mock.calls[0]?.[0]), "http://localhost");
    expect(url.searchParams.get("signal")).toBe("runaway_tool_loop_count");
    expect(url.searchParams.get("model")).toBe("gpt-4o");
  });
});
