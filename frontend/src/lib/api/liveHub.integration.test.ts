/// <reference types="node" />
import http from "node:http";
import { expect, it } from "vite-plus/test";
import { LiveHub, type LiveFrame } from "./liveHub.js";

it("recovers from a 503 and delivers readiness and updates", async () => {
  let requests = 0;
  const server = http.createServer((_req, res) => {
    requests++;
    if (requests === 1) {
      res.writeHead(503).end("unavailable");
      return;
    }
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    res.write(
      'event: events_ready\ndata: {}\n\nevent: data_changed\ndata: {"scope":"messages"}\n\n',
    );
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("server did not bind");
  const hub = new LiveHub();
  const frames: LiveFrame[] = [];
  try {
    await new Promise<void>((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error("stream did not recover")), 9000);
      hub.update(
        {
          postMessage: (frame) => {
            frames.push(frame);
            if (frame.type === "data_changed") {
              clearTimeout(timeout);
              resolve();
            }
          },
        },
        { base: `http://127.0.0.1:${address.port}`, token: "", credentialTime: 0 },
      );
    });
    expect(requests).toBe(2);
    expect(frames.map(({ type }) => type)).toEqual(["error", "events_ready", "data_changed"]);
    expect(frames.at(-1)?.data).toBe('{"scope":"messages"}');
  } finally {
    hub.close();
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  }
}, 12000);
