import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { watchSession } from "./client.js";
import { RemoteSyncService } from "./generated/index.js";

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

describe("shared session watch", () => {
  it("delivers updates through the generated authenticated request", async () => {
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
      expect(String(fetchMock.mock.calls[0]![0])).toBe(
        "http://localhost:3000/api/v1/events?session=session%2Fone",
      );
      const headers = new Headers(fetchMock.mock.calls[0]![1].headers);
      expect(headers.get("Authorization")).toBe("Bearer test-token");
      expect(headers.get("Accept")).toBe("text/event-stream");
    } finally {
      source.close();
    }
  });
  it("catches up after visibility changes without SharedWorker", async () => {
    vi.stubGlobal("SharedWorker", undefined);
    vi.spyOn(document, "hidden", "get").mockReturnValue(false);
    let controller!: ReadableStreamDefaultController<Uint8Array>;
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          new ReadableStream({
            start(c) {
              controller = c;
            },
          }),
          { headers: { "Content-Type": "text/event-stream" } },
        ),
      ),
    );
    const timing = vi.fn();
    const ready = vi.fn();
    const source = watchSession("fallback-session", vi.fn(), timing);
    source.addEventListener("session_ready", ready);
    const emit = (text: string) => controller.enqueue(new TextEncoder().encode(text));
    try {
      await vi.waitFor(() => expect(controller).toBeDefined());
      emit("event: session_ready\ndata: fallback-session\n\nevent: events_ready\ndata: {}\n\n");
      await vi.waitFor(() => expect(ready).toHaveBeenCalledTimes(1));
      source.setVisible(false);
      emit('event: session.timing\ndata: {"session_id":"fallback-session","turn_count":3}\n\n');
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(timing).not.toHaveBeenCalled();
      source.setVisible(true);
      await vi.waitFor(() => expect(ready).toHaveBeenCalledTimes(2));
      await vi.waitFor(() =>
        expect(timing).toHaveBeenCalledWith(expect.objectContaining({ turn_count: 3 })),
      );
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
