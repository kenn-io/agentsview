import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { getApiV1Version } from "./generated/metadata/metadata.js";
import { ApiError, orvalFetch, responseTimingOf, setAuthToken } from "./runtime.js";

describe("orvalFetch", () => {
  afterEach(() => {
    localStorage.clear();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("records when a JSON response was sent, answered, and read", async () => {
    vi.useFakeTimers({ toFake: ["performance"] });
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        vi.advanceTimersByTime(40);
        const response = new Response('{"ok":true}', {
          headers: { "Content-Type": "application/json" },
        });
        const text = response.text.bind(response);
        response.text = async () => {
          vi.advanceTimersByTime(15);
          return text();
        };
        return response;
      }),
    );
    const before = performance.now();

    const data = await orvalFetch<{ ok: boolean }>("/api/v1/usage/summary", {});

    expect(responseTimingOf(data)).toEqual({
      sentAt: before,
      headersAt: before + 40,
      bodyAt: before + 55,
    });
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
  it("tests a candidate server with only the supplied credentials", async () => {
    localStorage.setItem("agentsview-server-url", "https://selected.example.test");
    setAuthToken("selected-token");
    const fetchMock = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) =>
        new Response('{"version":"test"}', { headers: { "Content-Type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);
    await getApiV1Version({ baseUrl: "https://candidate.example.test" });
    await getApiV1Version({
      baseUrl: "https://candidate.example.test",
      headers: { Authorization: "Bearer candidate-token" },
    });
    expect(fetchMock.mock.calls.map(([input]) => input)).toEqual([
      "https://candidate.example.test/api/v1/version",
      "https://candidate.example.test/api/v1/version",
    ]);
    expect(new Headers(fetchMock.mock.calls[0]![1]?.headers).get("Authorization")).toBeNull();
    expect(new Headers(fetchMock.mock.calls[1]![1]?.headers).get("Authorization")).toBe(
      "Bearer candidate-token",
    );
  });
});

describe("API errors", () => {
  afterEach(() => {
    localStorage.clear();
    vi.unstubAllGlobals();
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

    await expect(orvalFetch("/api/v1/usage/summary", {})).rejects.toMatchObject({
      name: "ApiError",
      status: 400,
      code: "unknown_project_key",
      message: "unknown project key",
    } satisfies Partial<ApiError>);
  });
});
