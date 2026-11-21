import { EventSource } from "eventsource";
import { getApiV1Events, getGetApiV1EventsUrl } from "./generated/sessions/sessions.js";

export type LiveRequest = { base: string; token: string; credentialTime: number; session?: string };
export type LiveFrame = { type: string; data?: string };
export type LivePort = { postMessage: (frame: LiveFrame) => void };

export class LiveHub {
  private ports = new Map<LivePort, LiveRequest>();
  private source: EventSource | null = null;
  private sessions = new Set<string>();
  private ready = new Map<string, LiveFrame>();
  private timing = new Map<string, LiveFrame>();
  private credentials: { base: string; token: string } | null = null;
  private timer: ReturnType<typeof setTimeout> | null = null;

  update(port: LivePort, request: LiveRequest) {
    const previous = this.ports.get(port);
    this.ports.set(port, request);
    if (
      (!previous || previous.session !== request.session) &&
      this.source &&
      this.sameCredentials(request)
    ) {
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
    const request = [...this.ports.values()].reduce<LiveRequest | undefined>(
      (latest, candidate) =>
        !latest || candidate.credentialTime >= latest.credentialTime ? candidate : latest,
      undefined,
    );
    const changed =
      sessions.size !== this.sessions.size || [...sessions].some((id) => !this.sessions.has(id));
    if (this.source && request && !changed && this.sameCredentials(request)) return;
    this.source?.close();
    this.source = null;
    this.sessions = sessions;
    this.ready.clear();
    this.timing.clear();
    this.credentials = request ? { base: request.base, token: request.token } : null;
    if (!request) return;
    const params = { session: [...sessions] };
    const url = new URL(`${request.base}${getGetApiV1EventsUrl(params)}`);
    const source = new EventSource(url, {
      fetch: (_input, options) => {
        const headers = new Headers(options?.headers);
        if (request.token) headers.set("Authorization", `Bearer ${request.token}`);
        return getApiV1Events(params, { ...options, baseUrl: request.base, headers });
      },
    });
    this.source = source;
    let errors = 0;
    let opened = false;

    source.addEventListener("open", () => {
      if (this.source !== source) return;
      errors = 0;
      opened = true;
    });
    source.addEventListener("error", () => {
      if (this.source !== source) return;
      errors++;
      this.broadcast({ type: "error" });
      if (errors >= 5) {
        source.close();
        this.source = null;
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
        if (this.source !== source) return;
        errors = 0;
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
        this.broadcast(frame, session);
        if (type === "events_ready" || type === "session_ready" || type === "session_missing") {
          this.ready.set(session ?? "", frame);
          if (type === "session_missing") this.timing.delete(session ?? "");
        }
        if (type === "session.timing" && session) this.timing.set(session, frame);
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

  private sameCredentials(request?: LiveRequest) {
    return (
      !!request &&
      this.credentials?.base === request.base &&
      this.credentials.token === request.token
    );
  }
}
