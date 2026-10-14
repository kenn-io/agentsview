import { vi } from "vite-plus/test";

export function controlFrames() {
  const frames = new Map<number, FrameRequestCallback>();
  let frameId = 0;
  vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
    const id = ++frameId;
    frames.set(id, callback);
    return id;
  });
  vi.stubGlobal("cancelAnimationFrame", (id: number) => frames.delete(id));

  function flushFrames() {
    const pending = [...frames.values()];
    frames.clear();
    pending.forEach((callback) => callback(0));
  }

  return { frames, flushFrames };
}
