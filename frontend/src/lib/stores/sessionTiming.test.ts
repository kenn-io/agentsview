import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { sessionTiming } from "./sessionTiming.svelte.js";

const timingMocks = vi.hoisted(() => ({
  fetchSessionTiming: vi.fn(),
}));

vi.mock("../api/timing.js", () => ({
  fetchSessionTiming: timingMocks.fetchSessionTiming,
}));

beforeEach(() => {
  sessionTiming.reset();
  timingMocks.fetchSessionTiming.mockReset();
});

describe("SessionTimingStore", () => {
  it("aborts timing when another session replaces it", async () => {
    const signals: AbortSignal[] = [];
    timingMocks.fetchSessionTiming.mockImplementation((id, signal) => {
      signals.push(signal as AbortSignal);
      if (id === "s1") return new Promise(() => {});
      return Promise.resolve({ session_id: id });
    });

    void sessionTiming.load("s1");
    await Promise.resolve();
    await sessionTiming.load("s2");

    expect(signals[0]?.aborted).toBe(true);
  });

  it("aborts the current timing read on route exit", async () => {
    const signals: AbortSignal[] = [];
    timingMocks.fetchSessionTiming.mockImplementation((_id, signal) => {
      signals.push(signal as AbortSignal);
      return new Promise(() => {});
    });

    void sessionTiming.load("s1");
    await Promise.resolve();
    sessionTiming.cancelInFlight();

    expect(signals[0]?.aborted).toBe(true);
  });
});
