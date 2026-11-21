import { LiveHub, type LiveFrame, type LivePort, type LiveRequest } from "./liveHub.js";

type Peer = { seen: number; requests: Record<string, LiveRequest> };
type Message =
  | { type: "state"; id: string; requests: Record<string, LiveRequest> }
  | { type: "frame"; id: string; port: string; frame: LiveFrame };

// A tab advertises its subscriptions so another tab can take over when it closes.
export class LiveBroadcast {
  private id = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
  private send: (message: Message) => void;
  private requests: Record<string, LiveRequest> = {};
  private receivers = new Map<string, (frame: LiveFrame) => void>();
  private peers = new Map<string, Peer>();
  private hub: LiveHub | null = null;
  private ports = new Map<string, { port: LivePort; request: LiveRequest }>();
  private nextPort = 0;
  private election: ReturnType<typeof setTimeout> | null = null;

  constructor(base: string) {
    const receive = (message: Message) => {
      if (message.type === "frame") {
        if (message.id === this.id) this.receivers.get(message.port)?.(message.frame);
        return;
      }
      const known = this.peers.has(message.id);
      this.peers.set(message.id, { seen: Date.now(), requests: message.requests });
      if (!known) this.announce();
      this.elect();
    };
    const name = `agentsview-live:${base}`;
    if (typeof BroadcastChannel !== "undefined") {
      const channel = new BroadcastChannel(name);
      channel.onmessage = (event: MessageEvent<Message>) => receive(event.data);
      this.send = (message) => channel.postMessage(message);
    } else {
      window.addEventListener("storage", (event) => {
        if (event.key === name && event.newValue) receive(JSON.parse(event.newValue) as Message);
      });
      this.send = (message) => {
        localStorage.setItem(name, JSON.stringify(message));
        localStorage.removeItem(name);
      };
    }
    setInterval(() => {
      const cutoff = Date.now() - 4000;
      for (const [id, peer] of this.peers) if (peer.seen < cutoff) this.peers.delete(id);
      this.announce();
      this.elect();
    }, 1000);
    this.announce();
  }

  connect(receive: (frame: LiveFrame) => void): (value: LiveRequest | "leave" | "close") => void {
    const id = String(this.nextPort++);
    this.receivers.set(id, receive);
    return (value) => {
      if (value === "leave" || value === "close") {
        delete this.requests[id];
        if (value === "close") this.receivers.delete(id);
      } else this.requests[id] = value;
      this.announce();
      this.elect();
    };
  }

  private announce() {
    this.send({ type: "state", id: this.id, requests: this.requests });
  }

  private elect() {
    if (this.election !== null) return;
    this.election = setTimeout(() => {
      this.election = null;
      this.reconcile();
    }, 200);
  }

  private reconcile() {
    const participants = new Map(this.peers);
    participants.set(this.id, { seen: Date.now(), requests: this.requests });
    const leader = [...participants]
      .filter(([, peer]) => Object.keys(peer.requests).length)
      .map(([id]) => id)
      .sort()[0];
    if (leader !== this.id) {
      this.hub?.close();
      this.ports.clear();
      this.hub = null;
      return;
    }
    if (!this.hub) this.hub = new LiveHub();
    const wanted = new Map<string, { owner: string; id: string; request: LiveRequest }>();
    for (const [owner, peer] of participants) {
      for (const [id, request] of Object.entries(peer.requests))
        wanted.set(`${owner}:${id}`, { owner, id, request });
    }
    for (const [key, value] of this.ports) {
      if (!wanted.has(key)) {
        this.hub.leave(value.port);
        this.ports.delete(key);
      }
    }
    for (const [key, { owner, id, request }] of wanted) {
      const existing = this.ports.get(key);
      if (existing && JSON.stringify(existing.request) === JSON.stringify(request)) continue;
      const port = existing?.port ?? {
        postMessage: (frame: LiveFrame) => {
          if (owner === this.id) queueMicrotask(() => this.receivers.get(id)?.(frame));
          else this.send({ type: "frame", id: owner, port: id, frame });
        },
      };
      this.ports.set(key, { port, request });
      this.hub.update(port, request);
    }
  }
}
