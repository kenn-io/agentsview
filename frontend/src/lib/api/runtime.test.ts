import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { getApiV1Version } from "./generated/metadata/metadata.js";
import {
  ApiError,
  TransportError,
  isTransient,
  orvalFetch,
  getServerUrl,
  responseTimingOf,
  SERVER_URL_CHANGE_EVENT,
  setAuthToken,
  setServerUrl,
} from "./runtime.js";
import { requestUrl } from "../test-support/request-url.js";

describe("orvalFetch", () => {
  afterEach(() => {
    localStorage.clear();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("notifies this tab only when the selected server changes", () => {
    const servers: string[] = [];
    const listener = () => servers.push(getServerUrl());
    window.addEventListener(SERVER_URL_CHANGE_EVENT, listener);
    try {
      setServerUrl("https://example.com/remote");
      setServerUrl("https://example.com/remote");
      setServerUrl("");
      setServerUrl("");
      expect(servers).toEqual(["https://example.com/remote", ""]);
    } finally {
      window.removeEventListener(SERVER_URL_CHANGE_EVENT, listener);
    }
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
    expect(requestUrl(input)).toBe("https://example.test/api/v1/usage/summary");
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

  it("marks a fetch failure as transient", async () => {
    const cause = new TypeError("Failed to fetch");
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(cause));
    const error = await orvalFetch("/api/v1/search", {}).catch((err: unknown) => err);
    expect(error).toBeInstanceOf(TransportError);
    expect(error).toMatchObject({ message: cause.message, cause });
    expect(isTransient(error)).toBe(true);
  });

  it("rejects a malformed remote URL before fetch", async () => {
    setServerUrl("http://[");
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    try {
      const error = await orvalFetch("/api/v1/search", {}).catch((err: unknown) => err);
      expect(error).toBeInstanceOf(TypeError);
      expect(error).not.toBeInstanceOf(TransportError);
      expect(isTransient(error)).toBe(false);
      expect(fetchSpy).not.toHaveBeenCalled();
    } finally {
      fetchSpy.mockRestore();
    }
  });

  it("marks an interrupted response body as transient", async () => {
    const cause = new TypeError("Content-Length header of network response exceeds response Body.");
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          new ReadableStream({
            start(controller) {
              controller.error(cause);
            },
          }),
          { headers: { "Content-Type": "application/json" } },
        ),
      ),
    );
    const error = await orvalFetch("/api/v1/search", {}).catch((err: unknown) => err);
    expect(error).toBeInstanceOf(TransportError);
    expect(error).toMatchObject({ message: cause.message, cause });
    expect(isTransient(error)).toBe(true);
  });

  it("preserves a TypeError used as an abort reason", async () => {
    const abort = Object.assign(new TypeError("aborted"), { name: "AbortError" });
    const controller = new AbortController();
    controller.abort(abort);
    const error = await orvalFetch("/api/v1/search", { signal: controller.signal }).catch(
      (err: unknown) => err,
    );
    expect(error).toBe(abort);
    expect(isTransient(error)).toBe(false);
  });

  it("keeps invalid JSON permanent", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response("{", {
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );
    const error = await orvalFetch("/api/v1/search", {}).catch((err: unknown) => err);
    expect(error).toBeInstanceOf(SyntaxError);
    expect(isTransient(error)).toBe(false);
  });
});
