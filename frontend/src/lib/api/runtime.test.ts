import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { ApiError, callGenerated, orvalFetch, setAuthToken } from "./runtime.js";

describe("orvalFetch", () => {
  afterEach(() => {
    localStorage.clear();
    vi.unstubAllGlobals();
  });

  it("sends the selected server token with generated requests", async () => {
    localStorage.setItem("agentsview-server-url", "https://example.test");
    setAuthToken("secret");
    const fetchMock = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) =>
        new Response("{}", {
          headers: { "Content-Type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await orvalFetch("/api/v1/usage/summary", {});

    const [input, init] = fetchMock.mock.calls[0]!;
    expect(String(input)).toBe("https://example.test/api/v1/usage/summary");
    expect(new Headers(init?.headers).get("Authorization")).toBe("Bearer secret");
  });
});

describe("callGenerated", () => {
  afterEach(() => {
    localStorage.clear();
    vi.unstubAllGlobals();
  });

  it("passes its abort signal to the generated request", async () => {
    const controller = new AbortController();
    const request = vi.fn(async () => "done");

    await expect(callGenerated(request, controller.signal)).resolves.toBe("done");

    expect(request).toHaveBeenCalledWith({ signal: controller.signal });
  });

  it("normalizes generated API error bodies and codes", async () => {
    localStorage.setItem("agentsview-server-url", "http://localhost");
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            code: "unknown_project_key",
            error: "unknown project key",
          }),
          { status: 400, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );

    await expect(
      callGenerated(() => orvalFetch("/api/v1/usage/summary", {})),
    ).rejects.toMatchObject({
      name: "ApiError",
      status: 400,
      code: "unknown_project_key",
      message: "unknown project key",
    } satisfies Partial<ApiError>);
  });
});
