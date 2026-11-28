import { LiveHub, type LiveAck, type LiveRequest } from "./liveHub.js";

const hub = new LiveHub();
self.addEventListener("connect", ((event: MessageEvent) => {
  const port = event.ports[0]!;
  port.onmessage = (message: MessageEvent<LiveRequest | LiveAck | "leave">) => {
    if (message.data === "leave") hub.leave(port);
    else if ("type" in message.data) hub.acknowledge(port, message.data.receipt);
    else hub.update(port, message.data);
  };
  port.start();
}) as EventListener);
