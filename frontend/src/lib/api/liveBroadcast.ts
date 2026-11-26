import {
  LiveHub,
  type LiveAck,
  type LiveFrame,
  type LivePort,
  type LiveRequest,
} from "./liveHub.js";

type Peer = { seen: number; requests: Record<string, LiveRequest> };
type Message =
  | { type: "state"; id: string; requests: Record<string, LiveRequest> }
  | { type: "ack"; id: string; port: string; receipt: string }
  | { type: "frame"; from: string; id: string; port: string; frame: LiveFrame };

const ELECTION_DELAY_MS = 200;
const IDLE_RELEASE_MS = 1000;
const HEARTBEAT_INTERVAL_MS = 1000;
const PEER_TTL_MS = 4000;

// A tab advertises its subscriptions so another tab can take over when it closes.
export class LiveBroadcast {
  private id = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
  private base: string;
  private send: (message: Message) => void;
  private requests: Record<string, LiveRequest> = {};
  private receivers = new Map<string, (frame: LiveFrame) => void>();
  private peers = new Map<string, Peer>();
  private hub: LiveHub | null = null;
  private ports = new Map<string, { port: LivePort; request: LiveRequest }>();
  private nextPort = 0;
  private election: ReturnType<typeof setTimeout> | null = null;
  private heartbeat: ReturnType<typeof setInterval> | null = null;
  private useLocks =
    typeof navigator !== "undefined" && typeof navigator.locks?.request === "function";
  private lockRequested = false;
  private releaseLock: (() => void) | null = null;
  private releaseTimer: ReturnType<typeof setTimeout> | null = null;

  constructor(base: string) {
    this.base = base;
    const receive = (message: Message) => {
      if (message.type === "ack") {
        const port = this.ports.get(`${message.id}:${message.port}`)?.port;
        if (port) this.hub?.acknowledge(port, message.receipt);
        return;
      }
      if (message.type === "frame") {
        if (message.id !== this.id) return;
        if (this.hub) {
          // With Web Locks, the lock holder is the only leader, so a frame from
          // another hub is a stale leftover from the previous leader.
          if (!this.useLocks && message.from && message.from < this.id) this.stepDown();
          else {
            this.announce();
            return;
          }
        }
        this.receivers.get(message.port)?.(message.frame);
        return;
      }
      const known = this.peers.has(message.id);
      this.peers.set(message.id, { seen: Date.now(), requests: message.requests });
      if (!known) this.announce();
      if (this.useLocks) {
        if (this.releaseLock) this.syncHub();
        return;
      }
      if (this.hub && message.id < this.id && Object.keys(message.requests).length > 0) {
        this.stepDown();
      }
      this.elect();
    };
    const channel = new BroadcastChannel(`agentsview-live:${base}`);
    channel.onmessage = (event: MessageEvent<Message>) => receive(event.data);
    this.send = (message) => channel.postMessage(message);
    this.announce();
  }

  connect(
    receive: (frame: LiveFrame) => void,
  ): (value: LiveRequest | LiveAck | "leave" | "close") => void {
    const id = String(this.nextPort++);
    this.receivers.set(id, receive);
    return (value) => {
      if (typeof value === "object" && "type" in value) {
        const port = this.ports.get(`${this.id}:${id}`)?.port;
        if (port) this.hub?.acknowledge(port, value.receipt);
        else this.send({ type: "ack", id: this.id, port: id, receipt: value.receipt });
        return;
      }
      if (value === "leave" || value === "close") {
        delete this.requests[id];
        if (value === "close") this.receivers.delete(id);
      } else this.requests[id] = value;
      if (Object.keys(this.requests).length && this.heartbeat === null)
        this.heartbeat = setInterval(() => this.tick(), HEARTBEAT_INTERVAL_MS);
      else if (!Object.keys(this.requests).length && this.heartbeat !== null) {
        clearInterval(this.heartbeat);
        this.heartbeat = null;
      }
      this.tick();
    };
  }

  private tick() {
    const cutoff = Date.now() - PEER_TTL_MS;
    for (const [id, peer] of this.peers) if (peer.seen < cutoff) this.peers.delete(id);
    this.announce();
    if (this.useLocks) this.updateLock();
    else this.elect();
  }

  private announce() {
    this.send({ type: "state", id: this.id, requests: this.requests });
  }

  private updateLock() {
    const active = Object.keys(this.requests).length > 0;
    if (active) {
      if (this.releaseTimer !== null) {
        clearTimeout(this.releaseTimer);
        this.releaseTimer = null;
      }
      if (this.releaseLock) {
        this.syncHub();
        return;
      }
      if (this.lockRequested) return;
      this.lockRequested = true;
      void navigator.locks
        .request(`agentsview-live:${this.base}`, () => {
          if (Object.keys(this.requests).length === 0) {
            this.lockRequested = false;
            return;
          }
          return new Promise<void>((resolve) => {
            this.releaseLock = () => {
              this.releaseLock = null;
              this.lockRequested = false;
              this.stepDown();
              resolve();
            };
            this.syncHub();
          });
        })
        .catch(() => {
          this.lockRequested = false;
          this.releaseLock = null;
          this.useLocks = false;
          this.elect();
        });
      return;
    }
    if (this.releaseLock) {
      this.syncHub();
      if (this.releaseTimer === null) {
        this.releaseTimer = setTimeout(() => {
          this.releaseTimer = null;
          if (Object.keys(this.requests).length === 0) this.releaseLock?.();
        }, IDLE_RELEASE_MS);
      }
    }
  }

  private stepDown() {
    this.hub?.close();
    this.ports.clear();
    this.hub = null;
  }

  private elect() {
    if (this.election !== null) return;
    this.election = setTimeout(() => {
      this.election = null;
      this.reconcile();
    }, ELECTION_DELAY_MS);
  }

  private reconcile() {
    const participants = this.participants();
    const leader = [...participants]
      .filter(([, peer]) => Object.keys(peer.requests).length)
      .map(([id]) => id)
      .sort()[0];
    if (leader !== this.id) {
      this.stepDown();
      return;
    }
    this.syncHub(participants);
  }

  private participants() {
    const participants = new Map(this.peers);
    participants.set(this.id, { seen: Date.now(), requests: this.requests });
    return participants;
  }

  private syncHub(participants = this.participants()) {
    if (!this.hub) this.hub = new LiveHub();
    const wanted = new Map<string, { owner: string; id: string; request: LiveRequest }>();
    for (const [owner, peer] of participants) {
      for (const [id, request] of Object.entries(peer.requests))
        wanted.set(`${owner}:${id}`, { owner, id, request });
    }
    const hub = this.hub;
    hub.batch(() => {
      for (const [key, value] of this.ports) {
        if (!wanted.has(key)) {
          hub.leave(value.port);
          this.ports.delete(key);
        }
      }
      for (const [key, { owner, id, request }] of wanted) {
        const existing = this.ports.get(key);
        if (existing && JSON.stringify(existing.request) === JSON.stringify(request)) continue;
        const port = existing?.port ?? {
          postMessage: (frame: LiveFrame) => {
            if (owner === this.id) queueMicrotask(() => this.receivers.get(id)?.(frame));
            else this.send({ type: "frame", from: this.id, id: owner, port: id, frame });
          },
        };
        this.ports.set(key, { port, request });
        hub.update(port, request);
      }
    });
  }
}
