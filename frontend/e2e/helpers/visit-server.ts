import { createServer } from "node:http";

export const TOKEN = "visit-test-token";

export async function visitServer(upstream: string, authenticated = false, backCache = false) {
  const visits: {
    payload: unknown;
    authorization: string | undefined;
  }[] = [];
  const preflights: string[] = [];
  const server = createServer(async (req, res) => {
    res.setHeader("Access-Control-Allow-Origin", req.headers.origin ?? "*");
    res.setHeader("Access-Control-Allow-Headers", "Authorization, Content-Type");
    res.setHeader("Access-Control-Allow-Methods", "GET, POST, OPTIONS");
    res.setHeader("Access-Control-Max-Age", "0");
    if (req.method === "OPTIONS") {
      if (req.url === "/api/v1/telemetry/events")
        preflights.push(req.headers["access-control-request-headers"] as string);
      res.writeHead(204).end();
      return;
    }
    if (req.url === "/away") {
      res.setHeader("Content-Type", "text/html");
      res.end("<!doctype html><title>Away</title>");
      return;
    }
    // A finite event response lets the browser cache the page between visits.
    if (backCache && req.url === "/api/v1/events") {
      res.writeHead(204).end();
      return;
    }
    if (authenticated && req.headers.authorization !== `Bearer ${TOKEN}`) {
      res.writeHead(401).end();
      return;
    }
    try {
      const chunks: Buffer[] = [];
      for await (const chunk of req) chunks.push(Buffer.from(chunk));
      const body = Buffer.concat(chunks);
      const response =
        req.url === "/api/v1/telemetry/events"
          ? new Response('{"status":"queued"}', {
              status: 202,
              headers: { "Content-Type": "application/json" },
            })
          : await fetch(new URL(req.url!, upstream), {
              method: req.method,
              headers: {
                Origin: new URL(upstream).origin,
                ...(body.length ? { "Content-Type": "application/json" } : {}),
              },
              body: body.length ? body : undefined,
            });
      const bytes = Buffer.from(await response.arrayBuffer());
      const payload = body.length ? JSON.parse(body.toString()) : undefined;
      if (req.url === "/api/v1/telemetry/events" && payload?.event === "visit_ended") {
        visits.push({
          payload,
          authorization: req.headers.authorization,
        });
      }
      res.setHeader(
        "Content-Type",
        response.headers.get("Content-Type") ?? "application/octet-stream",
      );
      res.writeHead(response.status).end(bytes);
    } catch (error) {
      res.writeHead(502).end(String(error));
    }
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("Missing visit server address");
  return {
    url: `http://127.0.0.1:${address.port}`,
    visits,
    preflights,
    close: async () => {
      server.closeAllConnections();
      await new Promise<void>((resolve, reject) =>
        server.close((error) => (error ? reject(error) : resolve())),
      );
    },
  };
}
