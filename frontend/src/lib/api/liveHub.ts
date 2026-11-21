import { EventSource } from "eventsource";

export type LiveRequest = { base: string; token: string; session?: string; visible: boolean };
export type LiveFrame = { type: string; data?: string };
export type LivePort = { postMessage: (frame: LiveFrame) => void };

type Connection = { source: EventSource; sessions: Set<string>; ready: boolean };

export class LiveHub {
  private ports = new Map<LivePort, LiveRequest>();
  private current: Connection | null = null;
  private pending: Connection | null = null;
  private hideTimer: ReturnType<typeof setTimeout> | null = null;

  update(port: LivePort, request: LiveRequest) {
    const previous = this.ports.get(port);
    this.ports.set(port, request);
    if (
      request.visible &&
      (!previous || !previous.visible) &&
      this.current?.ready &&
      (!request.session || this.current.sessions.has(request.session))
    ) {
      port.postMessage({
        type: request.session ? "session_ready" : "events_ready",
        data: request.session,
      });
    }
    this.reconcile();
  }

  leave(port: LivePort) {
    this.ports.delete(port);
    this.reconcile();
  }

  private reconcile() {
    const requests = [...this.ports.values()];
    if (!requests.some((request) => request.visible)) {
      if (!this.hideTimer)
        this.hideTimer = setTimeout(() => {
          this.hideTimer = null;
          this.current?.source.close();
          this.pending?.source.close();
          this.current = this.pending = null;
        }, 1000);
      return;
    }
    if (this.hideTimer) clearTimeout(this.hideTimer);
    this.hideTimer = null;
    const sessions = new Set(requests.flatMap((r) => (r.session ? [r.session] : [])));
    const same = (connection: Connection | null) =>
      connection &&
      connection.sessions.size === sessions.size &&
      [...sessions].every((id) => connection.sessions.has(id));
    if (same(this.pending) || (!this.pending && same(this.current))) return;
    this.pending?.source.close();
    const request = requests.find((r) => r.visible)!;
    const url = new URL(`${request.base}/api/v1/events`);
    for (const id of sessions) url.searchParams.append("session", id);
    const source = new EventSource(url, {
      fetch: (input, options) => {
        const headers = new Headers(options?.headers);
        if (request.token) headers.set("Authorization", `Bearer ${request.token}`);
        return fetch(input, { ...options, headers });
      },
    });
    const connection: Connection = { source, sessions, ready: false };
    this.pending = connection;
    const buffered: Array<{ frame: LiveFrame; id?: string }> = [];
    let errors = 0;
    let opened = false;
    source.addEventListener("open", () => {
      errors = 0;
      opened = true;
    });
    source.addEventListener("error", () => {
      errors++;
      this.broadcast({ type: "error" });
      if (errors >= 5) {
        source.close();
        if (this.pending === connection) this.pending = null;
        if (this.current === connection) this.current = null;
        this.broadcast({ type: opened ? "closed" : "permanent_failure" });
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
        errors = 0;
        const data = (event as MessageEvent).data as string;
        if (type === "events_ready") {
          const old = this.current;
          if (this.pending === connection) {
            this.pending = null;
            this.current = connection;
            connection.ready = true;
            old?.source.close();
            if (old) {
              for (const item of buffered) {
                if (item.id && !old.sessions.has(item.id)) this.broadcast(item.frame, item.id);
              }
              return;
            }
          }
          this.broadcast({ type });
          return;
        }
        let id: string | undefined;
        if (type.startsWith("session_")) id = data;
        if (type === "session.timing") {
          try {
            id = (JSON.parse(data) as { session_id: string }).session_id;
          } catch {
            return;
          }
        }
        if (this.pending === connection && this.current)
          buffered.push({ frame: { type, data }, id });
        else this.broadcast({ type, data }, id);
      });
    }
  }

  private broadcast(frame: LiveFrame, session?: string) {
    for (const [port, request] of this.ports) {
      if (!request.visible) continue;
      if (
        session !== undefined
          ? request.session !== session
          : !!request.session && frame.type === "data_changed"
      )
        continue;
      if (frame.type === "events_ready" && request.session) continue;
      port.postMessage(frame);
    }
  }
}
