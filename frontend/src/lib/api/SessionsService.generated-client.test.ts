import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { SessionsService } from "./generated/index.js";

describe("SessionsService path parameters", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  it("keeps an opaque session ID in one URL path segment", async () => {
    const fetchMock = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) =>
        new Response("{}", {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await SessionsService.getApiV1SessionsById({ id: "deepseek-harness:child%7E/%25?#" });

    expect(String(fetchMock.mock.calls[0]?.[0])).toBe(
      "/api/v1/sessions/deepseek-harness%3Achild%257E%2F%2525%3F%23",
    );
  });
});
