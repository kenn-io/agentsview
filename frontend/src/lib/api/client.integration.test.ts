import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { watchSession } from "./client.js";
import { RemoteSyncService } from "./generated/index.js";

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

describe("shared session watch", () => {
  it("delivers updates through the authenticated in-page fallback", async () => {
    vi.stubGlobal("SharedWorker", undefined);
    localStorage.setItem("agentsview-auth-token", "test-token");
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        new ReadableStream({
          start(controller) {
            controller.enqueue(
              new TextEncoder().encode("event: session_updated\ndata: session/one\n\n"),
            );
          },
        }),
        { headers: { "Content-Type": "text/event-stream" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(document, "hidden", "get").mockReturnValue(false);
    const onUpdate = vi.fn();
    const source = watchSession("session/one", onUpdate);
    try {
      await vi.waitFor(() => expect(onUpdate).toHaveBeenCalledOnce());
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
