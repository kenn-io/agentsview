import { describe, expect, it } from "vite-plus/test";
import { LatestRead } from "./latest-read.js";

describe("LatestRead", () => {
  it("aborts the previous read when a replacement begins", () => {
    const slot = new LatestRead();
    const first = slot.begin();
    const second = slot.begin();

    expect(first.aborted).toBe(true);
    expect(second.aborted).toBe(false);
    expect(slot.isCurrent(second)).toBe(true);
  });

  it("invalidates a read when its surface is cancelled", () => {
    const slot = new LatestRead();
    const signal = slot.begin();

    slot.cancel();

    expect(signal.aborted).toBe(true);
    expect(slot.isCurrent(signal)).toBe(false);
  });

  it("does not let stale completion clear a newer read", () => {
    const slot = new LatestRead();
    const first = slot.begin();
    const second = slot.begin();

    expect(slot.finish(first)).toBe(false);
    expect(slot.isCurrent(second)).toBe(true);
    expect(slot.finish(second)).toBe(true);
    expect(slot.isCurrent(second)).toBe(false);
  });
});
