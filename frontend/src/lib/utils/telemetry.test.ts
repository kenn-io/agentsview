import { describe, it, expect, vi, beforeEach, afterEach } from "vite-plus/test";
import { setAuthToken, setServerUrl } from "../api/runtime.js";
import { reportTelemetry } from "./telemetry.js";

describe("reportTelemetry", () => {
  let originalFetch: typeof globalThis.fetch;
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    setAuthToken("");
    setServerUrl("");
    originalFetch = globalThis.fetch;
    fetchMock = vi.fn().mockImplementation(
      async () =>
        new Response('{"status":"queued"}', {
          status: 202,
          headers: { "Content-Type": "application/json" },
        }),
    );
    globalThis.fetch = fetchMock as unknown as typeof globalThis.fetch;
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    setAuthToken("");
    setServerUrl("");
    vi.restoreAllMocks();
  });

  async function settle() {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }

  it("posts the event and its property as JSON", () => {
    reportTelemetry("export_run", { format: "csv" });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("/api/v1/telemetry/events");
    expect(init.method).toBe("POST");
    expect(new Headers(init.headers).get("Content-Type")).toBe("application/json");
    expect(init.body).toBe('{"event":"export_run","properties":{"format":"csv"}}');
  });

  it("posts app_opened without a properties key", () => {
    reportTelemetry("app_opened");

    expect(fetchMock.mock.calls[0]![1].body).toBe('{"event":"app_opened"}');
  });

  it("uses the remote server URL and bearer token", () => {
    setServerUrl("http://remote.example:8080");
    setAuthToken("tok");
    reportTelemetry("search_run", { query_type: "text" });

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://remote.example:8080/api/v1/telemetry/events");
    expect(new Headers(init.headers).get("Authorization")).toBe("Bearer tok");
  });

  it("swallows a rejected fetch, including a non-Error rejection", async () => {
    fetchMock.mockRejectedValueOnce(new Error("offline"));
    fetchMock.mockRejectedValueOnce("network down");
    // Vitest fails the run on an unhandled rejection, so settling the tick is the assertion.
    expect(() => reportTelemetry("session_viewed", { agent: "codex" })).not.toThrow();
    expect(() => reportTelemetry("session_viewed", { agent: "codex" })).not.toThrow();
    await expect(settle()).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("swallows a 503 response", async () => {
    fetchMock.mockResolvedValueOnce(new Response("unavailable", { status: 503 }));
    reportTelemetry("analytics_viewed", { page: "usage" });
    await expect(settle()).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
