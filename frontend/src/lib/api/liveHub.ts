import { EventSource } from "eventsource";

export type LiveRequest = { base: string; token: string; session?: string };
export type LiveFrame = { type: string; data?: string };
export type LivePort = { postMessage: (frame: LiveFrame) => void };

export class LiveHub {
  private ports = new Map<LivePort, LiveRequest>();
  private source: EventSource | null = null;
  private sessions = new Set<string>();
  private ready = new Map<string, LiveFrame>();
  private timing = new Map<string, LiveFrame>();
  private errors = 0;
  private opened = false;
  private timer: ReturnType<typeof setTimeout> | null = null;

  update(port: LivePort, request: LiveRequest) {
    const previous = this.ports.get(port);
    this.ports.set(port, request);
    if ((!previous || previous.session !== request.session) && this.source) {
      const key = request.session ?? "";
      const ready = this.ready.get(key);
      if (ready) port.postMessage(ready);
      const timing = this.timing.get(key);
      if (timing) port.postMessage(timing);
    }
    this.schedule();
  }

  leave(port: LivePort) {
    this.ports.delete(port);
    this.schedule();
  }

  close() {
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
    this.source?.close();
    this.source = null;
    this.ports.clear();
  }

  private schedule() {
    if (this.timer !== null) clearTimeout(this.timer);
    if (!this.source && this.ports.size > 0) {
      this.reconcile();
    } else {
      this.timer = setTimeout(() => {
        this.timer = null;
        this.reconcile();
      }, 250);
    }
  }

  private reconcile() {
    const sessions = new Set(
      [...this.ports.values()].flatMap((request) => (request.session ? [request.session] : [])),
    );
    const changed =
      sessions.size !== this.sessions.size || [...sessions].some((id) => !this.sessions.has(id));
    if (this.source && this.ports.size && !changed) return;
    this.source?.close();
    this.source = null;
    this.sessions = sessions;
    this.ready.clear();
    this.timing.clear();
    const request = this.ports.values().next().value;
    if (!request) return;
    const url = new URL(`${request.base}/api/v1/events`);
    for (const id of sessions) url.searchParams.append("session", id);
    const source = new EventSource(url, {
      fetch: (input, options) => {
        const headers = new Headers(options?.headers);
        if (request.token) headers.set("Authorization", `Bearer ${request.token}`);
        return fetch(input, { ...options, headers });
      },
    });
    this.source = source;

    source.addEventListener("open", () => {
      if (this.source !== source) return;
      this.errors = 0;
      this.opened = true;
    });
    source.addEventListener("error", () => {
      if (this.source !== source) return;
      this.errors++;
      this.broadcast({ type: "error" });
      if (this.errors >= 5) {
        source.close();
        this.source = null;
        this.broadcast({ type: this.opened ? "closed" : "permanent_failure" });
      } else if (source.readyState === 2) {
        this.source = null;
        this.broadcast({ type: "closed" });
        this.timer = setTimeout(() => {
          this.timer = null;
          this.reconcile();
        }, 1000);
      }
    });
    for (const type of [
      "events_ready",
      "data_changed",
      "session_ready",
      "session_updated",
      "session_missing",
      "session.timing",
    ]) {
      source.addEventListener(type, (event) => {
        if (this.source !== source) return;
        this.errors = 0;
        const data = (event as MessageEvent).data as string;
        let session: string | undefined;
        if (type.startsWith("session_")) session = data;
        if (type === "session.timing") {
          try {
            session = (JSON.parse(data) as { session_id: string }).session_id;
          } catch {
            return;
          }
        }
        const frame = { type, data };
        if (type === "events_ready" || type === "session_ready" || type === "session_missing") {
          this.ready.set(session ?? "", frame);
          if (type === "session_missing") this.timing.delete(session ?? "");
        }
        if (type === "session.timing" && session) this.timing.set(session, frame);
        this.broadcast(frame, session);
      });
    }
  }

  private broadcast(frame: LiveFrame, session?: string) {
    for (const [port, request] of this.ports) {
      if (session !== undefined && request.session !== session) continue;
      if ((frame.type === "events_ready" || frame.type === "data_changed") && request.session)
        continue;
      port.postMessage(frame);
    }
  }
}
