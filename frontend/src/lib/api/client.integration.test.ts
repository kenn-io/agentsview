import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { watchSession } from "./client.js";
import { RemoteSyncService } from "./generated/index.js";

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

describe("generated session watch", () => {
  it("delivers updates through the generated authenticated request", async () => {
    localStorage.setItem("agentsview-auth-token", "test-token");
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        new ReadableStream({
          start(controller) {
            controller.enqueue(new TextEncoder().encode("event: session_updated\ndata: {}\n\n"));
          },
        }),
        { headers: { "Content-Type": "text/event-stream" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    const onUpdate = vi.fn();
    const source = watchSession("session/one", onUpdate);
    try {
      await vi.waitFor(() => expect(onUpdate).toHaveBeenCalledOnce());
      expect(fetchMock).toHaveBeenCalledWith(
        "/api/v1/sessions/session%2Fone/watch",
        expect.objectContaining({ method: "GET" }),
      );
      const headers = new Headers(fetchMock.mock.calls[0]![1].headers);
      expect(headers.get("Authorization")).toBe("Bearer test-token");
      expect(headers.get("Accept")).toBe("text/event-stream");
    } finally {
      source.close();
    }
  });
});

describe("generated binary downloads", () => {
  it("preserves remote archive bytes", async () => {
    const bytes = new Uint8Array([0, 255, 254, 1]);
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValue(
          new Response(bytes, { headers: { "Content-Type": "application/x-tar" } }),
        ),
    );

    const response = await RemoteSyncService.postApiV1RemoteSyncArchive(
      { dirs: {} },
      { headers: { "X-AgentsView-Remote-Sync-Version": "1" } },
    );

    expect(Array.from(new Uint8Array(await response.arrayBuffer()))).toEqual(Array.from(bytes));
  });
});
