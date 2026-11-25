import { EventSource } from "eventsource";
import { getApiV1Events, getGetApiV1EventsUrl } from "./generated/sessions/sessions.js";

export type LiveRequest = { base: string; token: string; credentialTime: number; session?: string };
export type LiveFrame = { type: string; data?: string; id?: string };
export type LivePort = { postMessage: (frame: LiveFrame) => void };

export class LiveHub {
  private ports = new Map<LivePort, LiveRequest>();
  private source: EventSource | null = null;
  private previous: EventSource | null = null;
  private continuous = false;
  private synced = new Map<LivePort, { archive?: boolean; marker?: string; timing?: string }>();
  private sessions = new Set<string>();
  private ready = new Map<string, LiveFrame>();
  private timing = new Map<string, LiveFrame>();
  private credentials: { base: string; token: string } | null = null;
  private connected = false;
  private timer: ReturnType<typeof setTimeout> | null = null;

  update(port: LivePort, request: LiveRequest) {
    const previous = this.ports.get(port);
    this.ports.set(port, request);
    if (!previous || previous.session !== request.session) {
      this.synced.delete(port);
    }
    if (
      (!previous || previous.session !== request.session) &&
      this.source &&
      this.sameCredentials(request)
    ) {
      const key = request.session ?? "";
      const ready = this.ready.get(key);
      if (ready?.type === "session_missing") {
        this.ready.delete(key);
        this.sessions.delete(key);
      } else if (ready) this.deliver(port, ready);
      const timing = this.timing.get(key);
      if (timing) this.deliver(port, timing);
    }
    if (
      this.source &&
      !this.sameCredentials(request) &&
      [...this.ports.values()].every(
        (candidate) => candidate.credentialTime <= request.credentialTime,
      )
    ) {
      if (this.timer !== null) clearTimeout(this.timer);
      this.timer = null;
      this.reconcile();
      return;
    }
    this.schedule();
  }

  leave(port: LivePort) {
    const session = this.ports.get(port)?.session;
    if (session && this.ready.get(session)?.type === "session_missing") {
      this.ready.delete(session);
      this.sessions.delete(session);
    }
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
    this.connected = false;
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
    const old = this.previous ?? this.source;
    this.connected = this.sameCredentials(request) && (this.connected || old?.readyState === 1);
    if (!request || !this.sameCredentials(request) || old?.readyState !== 1) {
      this.source?.close();
      this.previous?.close();
      this.previous = null;
      this.continuous = false;
    } else if (this.previous) {
      this.source?.close();
    } else {
      this.previous = this.source;
      this.continuous = this.ready.has("");
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
        headers.delete("Last-Event-ID");
        if (request.token) headers.set("Authorization", `Bearer ${request.token}`);
        return getApiV1Events(params, { ...options, baseUrl: request.base, headers });
      },
    });
    this.source = source;
    let errors = 0;
    let opened = this.connected;
    let suppressReady = false;

    source.addEventListener("open", () => {
      if (this.source !== source && this.previous !== source) return;
      errors = 0;
      opened = true;
      this.connected = true;
    });
    source.addEventListener("error", () => {
      if (this.source !== source && this.previous !== source) return;
      errors++;
      if (this.previous === source || !this.previous) {
        this.continuous = false;
        this.synced.clear();
        this.broadcast({ type: "error" });
      } else {
        for (const state of this.synced.values()) {
          delete state.marker;
          delete state.timing;
        }
      }
      suppressReady = false;
      if (this.previous === source) {
        source.close();
        this.previous = null;
        return;
      }
      if (errors >= 5) {
        source.close();
        this.previous?.close();
        this.previous = null;
        this.source = null;
        this.connected = false;
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
        if (this.source !== source && this.previous !== source) return;
        if (this.source === source && this.previous) {
          // The server subscribes to archive updates before sending its first frame.
          suppressReady = this.continuous && this.previous.readyState === 1;
          if (!suppressReady) this.synced.clear();
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
        const frame: LiveFrame = { type, data };
        if (type === "session_ready" || type === "session_updated")
          frame.id = (event as MessageEvent).lastEventId;
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
      const state = this.synced.get(port);
      if (suppressSynced && state?.archive) continue;
      if (frame.type === "session_ready" && frame.id && frame.id === state?.marker) continue;
      if (frame.type === "session.timing" && frame.data === state?.timing) continue;
      this.deliver(port, frame);
    }
  }

  private deliver(port: LivePort, frame: LiveFrame) {
    const state = this.synced.get(port) ?? {};
    if (frame.type === "events_ready") state.archive = true;
    if (frame.type === "session_ready" || frame.type === "session_updated") state.marker = frame.id;
    if (frame.type === "session.timing") state.timing = frame.data;
    if (frame.type === "session_missing") {
      delete state.marker;
      delete state.timing;
    }
    this.synced.set(port, state);
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
