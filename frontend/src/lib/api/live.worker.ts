import { LiveHub, type LiveRequest } from "./liveHub.js";

const hub = new LiveHub();
self.addEventListener("connect", ((event: MessageEvent) => {
  const port = event.ports[0]!;
  port.onmessage = (message: MessageEvent<LiveRequest | "leave">) => {
    if (message.data === "leave") hub.leave(port);
    else hub.update(port, message.data);
  };
  port.start();
}) as EventListener);
