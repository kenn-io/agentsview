import { EventSource } from "eventsource";
import { getApiV1Events, getGetApiV1EventsUrl } from "./generated/sessions/sessions.js";

export type LiveRequest = { base: string; token: string; credentialTime: number; session?: string };
export type LiveFrame = { type: string; data?: string };
export type LivePort = { postMessage: (frame: LiveFrame) => void };

export class LiveHub {
  private ports = new Map<LivePort, LiveRequest>();
  private source: EventSource | null = null;
  private previous: EventSource | null = null;
  private continuous = false;
  private synced = new Set<LivePort>();
  private sessions = new Set<string>();
  private ready = new Map<string, LiveFrame>();
  private timing = new Map<string, LiveFrame>();
  private credentials: { base: string; token: string } | null = null;
  private timer: ReturnType<typeof setTimeout> | null = null;

  update(port: LivePort, request: LiveRequest) {
    const previous = this.ports.get(port);
    this.ports.set(port, request);
    if (!previous || previous.session !== request.session) this.synced.delete(port);
    if (
      (!previous || previous.session !== request.session) &&
      this.source &&
      this.sameCredentials(request)
    ) {
      const key = request.session ?? "";
      const ready = this.ready.get(key);
      if (ready) this.deliver(port, ready);
      const timing = this.timing.get(key);
      if (timing) port.postMessage(timing);
    }
    this.schedule();
  }

  leave(port: LivePort) {
    this.ports.delete(port);
    this.synced.delete(port);
    this.schedule();
  }

  close() {
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
    this.source?.close();
    this.previous?.close();
    this.source = null;
    this.previous = null;
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
    if (this.previous) {
      this.source?.close();
    } else {
      this.previous = this.source;
      this.continuous =
        this.previous !== null && this.ready.has("") && this.credentials?.base === request?.base;
    }
    this.source = null;
    if (!this.previous) {
      this.ready.clear();
      this.timing.clear();
      this.synced.clear();
    }
    this.sessions = sessions;
    this.credentials = request ? { base: request.base, token: request.token } : null;
    if (!request) {
      this.previous?.close();
      this.previous = null;
      this.ready.clear();
      this.timing.clear();
      this.synced.clear();
      return;
    }
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
    let suppressReady = false;

    source.addEventListener("open", () => {
      if (this.source !== source && this.previous !== source) return;
      errors = 0;
      opened = true;
    });
    source.addEventListener("error", () => {
      if (this.source !== source && this.previous !== source) return;
      errors++;
      if (this.previous === source || !this.previous) {
        this.continuous = false;
        this.synced.clear();
        this.broadcast({ type: "error" });
      }
      suppressReady = false;
      if (errors >= 5) {
        source.close();
        if (this.previous === source) {
          this.previous = null;
        } else {
          this.previous?.close();
          this.previous = null;
          this.source = null;
          this.broadcast({ type: opened ? "closed" : "permanent_failure" });
        }
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
        if (this.source !== source && this.previous !== source) return;
        if (this.source === source && this.previous) {
          // The server subscribes to archive updates before sending its first frame.
          suppressReady = this.continuous;
          this.previous.close();
          this.previous = null;
          this.ready.clear();
          this.timing.clear();
        }
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
        this.broadcast(frame, session, suppressReady && type === "events_ready");
        if (type === "events_ready") suppressReady = false;
        if (type === "events_ready" || type === "session_ready" || type === "session_missing") {
          this.ready.set(session ?? "", frame);
          if (type === "session_missing") this.timing.delete(session ?? "");
        }
        if (type === "session.timing" && session) this.timing.set(session, frame);
      });
    }
  }

  private broadcast(frame: LiveFrame, session?: string, suppressSynced = false) {
    for (const [port, request] of this.ports) {
      if (session !== undefined && request.session !== session) continue;
      if ((frame.type === "events_ready" || frame.type === "data_changed") && request.session)
        continue;
      if (suppressSynced && this.synced.has(port)) continue;
      this.deliver(port, frame);
    }
  }

  private deliver(port: LivePort, frame: LiveFrame) {
    if (frame.type === "events_ready") this.synced.add(port);
    port.postMessage(frame);
  }

  private sameCredentials(request?: LiveRequest) {
    return (
      !!request &&
      this.credentials?.base === request.base &&
      this.credentials.token === request.token
    );
  }
}
