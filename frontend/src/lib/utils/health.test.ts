import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { setupVisibilityHealthCheck } from "./health.js";
import { setAuthToken, setServerUrl } from "../api/runtime.js";

describe("setupVisibilityHealthCheck", () => {
  let originalFetch: typeof globalThis.fetch;
  let reloadSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    originalFetch = globalThis.fetch;
    reloadSpy = vi.fn();
    Object.defineProperty(window, "location", {
      value: { reload: reloadSpy },
      writable: true,
    });
    setAuthToken("");
    setServerUrl("");
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    setAuthToken("");
    setServerUrl("");
    vi.restoreAllMocks();
  });

  function fireVisible() {
    Object.defineProperty(document, "visibilityState", {
      value: "visible",
      configurable: true,
    });
    document.dispatchEvent(new Event("visibilitychange"));
  }

  function fireHidden() {
    Object.defineProperty(document, "visibilityState", {
      value: "hidden",
      configurable: true,
    });
    document.dispatchEvent(new Event("visibilitychange"));
  }

  it("reloads when backend is unreachable", async () => {
    globalThis.fetch = vi.fn().mockRejectedValue(new Error("net"));
    const cleanup = setupVisibilityHealthCheck(() => "/api/v1");
    fireVisible();
    await new Promise((r) => setTimeout(r, 50));
    expect(reloadSpy).toHaveBeenCalledOnce();
    cleanup();
  });

  it("does not reload when backend responds OK", async () => {
    globalThis.fetch = vi
      .fn()
      .mockResolvedValue(new Response("{}", { status: 200 }));
    const cleanup = setupVisibilityHealthCheck(() => "/api/v1");
    fireVisible();
    await new Promise((r) => setTimeout(r, 50));
    expect(reloadSpy).not.toHaveBeenCalled();
    cleanup();
  });

  it("skips check when document is hidden", async () => {
    globalThis.fetch = vi.fn().mockRejectedValue(new Error("net"));
    const cleanup = setupVisibilityHealthCheck(() => "/api/v1");
    fireHidden();
    await new Promise((r) => setTimeout(r, 50));
    expect(globalThis.fetch).not.toHaveBeenCalled();
    cleanup();
  });

  it("debounces rapid visibility changes", async () => {
    globalThis.fetch = vi
      .fn()
      .mockResolvedValue(new Response("{}", { status: 200 }));
    const cleanup = setupVisibilityHealthCheck(() => "/api/v1");
    fireVisible();
    fireVisible();
    fireVisible();
    await new Promise((r) => setTimeout(r, 50));
    expect(globalThis.fetch).toHaveBeenCalledOnce();
    cleanup();
  });

  it("does not reload on 5xx server error", async () => {
    const onBackendDegraded = vi.fn();
    globalThis.fetch = vi
      .fn()
      .mockResolvedValue(new Response("", { status: 502 }));
    const cleanup = setupVisibilityHealthCheck(() => "/api/v1", {
      onBackendDegraded,
    });
    fireVisible();
    await new Promise((r) => setTimeout(r, 50));
    expect(reloadSpy).not.toHaveBeenCalled();
    expect(onBackendDegraded).toHaveBeenCalledWith(502);
    cleanup();
  });

  it("does not reload on 401 (backend alive, auth needed)", async () => {
    globalThis.fetch = vi
      .fn()
      .mockResolvedValue(new Response("", { status: 401 }));
    const cleanup = setupVisibilityHealthCheck(() => "/api/v1");
    fireVisible();
    await new Promise((r) => setTimeout(r, 50));
    expect(reloadSpy).not.toHaveBeenCalled();
    cleanup();
  });

  it("does not reload on 403", async () => {
    globalThis.fetch = vi
      .fn()
      .mockResolvedValue(new Response("", { status: 403 }));
    const cleanup = setupVisibilityHealthCheck(() => "/api/v1");
    fireVisible();
    await new Promise((r) => setTimeout(r, 50));
    expect(reloadSpy).not.toHaveBeenCalled();
    cleanup();
  });

  it("removes listener on cleanup", async () => {
    globalThis.fetch = vi.fn().mockRejectedValue(new Error("net"));
    const cleanup = setupVisibilityHealthCheck(() => "/api/v1");
    cleanup();
    fireVisible();
    await new Promise((r) => setTimeout(r, 50));
    expect(globalThis.fetch).not.toHaveBeenCalled();
  });

  it("resolves base URL lazily on each check", async () => {
    let base = "/first";
    globalThis.fetch = vi
      .fn()
      .mockResolvedValue(new Response("{}", { status: 200 }));
    const cleanup = setupVisibilityHealthCheck(() => base);
    fireVisible();
    await new Promise((r) => setTimeout(r, 50));
    expect(globalThis.fetch).toHaveBeenCalledWith(
      "/first/version",
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    cleanup();
  });

  it("includes auth header when token is set", async () => {
    setAuthToken("test-secret");
    globalThis.fetch = vi
      .fn()
      .mockResolvedValue(new Response("{}", { status: 200 }));
    const cleanup = setupVisibilityHealthCheck(() => "/api/v1");
    fireVisible();
    await new Promise((r) => setTimeout(r, 50));
    expect(globalThis.fetch).toHaveBeenCalledWith(
      "/api/v1/version",
      expect.objectContaining({
        headers: { Authorization: "Bearer test-secret" },
      }),
    );
    cleanup();
  });

  it("omits auth header when no token", async () => {
    globalThis.fetch = vi
      .fn()
      .mockResolvedValue(new Response("{}", { status: 200 }));
    const cleanup = setupVisibilityHealthCheck(() => "/api/v1");
    fireVisible();
    await new Promise((r) => setTimeout(r, 50));
    const call = (globalThis.fetch as ReturnType<typeof vi.fn>)
      .mock.calls[0]!;
    const init = call[1] as RequestInit;
    expect(init.headers).toBeUndefined();
    cleanup();
  });

  it("skips health check in desktop mode with local sidecar", async () => {
    Object.defineProperty(window, "location", {
      value: { reload: reloadSpy, search: "?desktop=1" },
      writable: true,
    });
    globalThis.fetch = vi.fn().mockRejectedValue(new Error("net"));
    const cleanup = setupVisibilityHealthCheck(() => "/api/v1");
    fireVisible();
    await new Promise((r) => setTimeout(r, 50));
    expect(globalThis.fetch).not.toHaveBeenCalled();
    expect(reloadSpy).not.toHaveBeenCalled();
    cleanup();
  });

  it("keeps health check in desktop mode with remote server", async () => {
    Object.defineProperty(window, "location", {
      value: { reload: reloadSpy, search: "?desktop=1" },
      writable: true,
    });
    setServerUrl("http://remote:8080");
    globalThis.fetch = vi.fn().mockRejectedValue(new Error("net"));
    const cleanup = setupVisibilityHealthCheck(() => "/api/v1");
    fireVisible();
    await new Promise((r) => setTimeout(r, 50));
    expect(globalThis.fetch).toHaveBeenCalled();
    expect(reloadSpy).toHaveBeenCalledOnce();
    cleanup();
  });
});
