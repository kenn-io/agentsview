import { describe, it, expect, beforeEach, vi } from "vite-plus/test";
import { PinsService } from "../api/generated/index";
import { createPinsStore } from "./pins.svelte.js";

const apiRuntimeMocks = vi.hoisted(() => ({
  callGenerated: vi.fn(
    (request: () => Promise<unknown>, _signal?: AbortSignal) => request(),
  ),
}));

vi.mock("../api/runtime.js", () => ({
  configureGeneratedClient: vi.fn(),
  callGenerated: apiRuntimeMocks.callGenerated,
  isAbortError: vi.fn(() => false),
}));

vi.mock("../api/generated/index", () => ({
  PinsService: {
    getApiV1Pins: vi.fn().mockResolvedValue({ pins: [] }),
    getApiV1SessionsIdPins: vi.fn().mockResolvedValue({ pins: [] }),
    postApiV1SessionsIdMessagesMessageidPin: vi.fn().mockResolvedValue({ id: 1 }),
    deleteApiV1SessionsIdMessagesMessageidPin: vi.fn().mockResolvedValue(undefined),
  },
}));

const pinsService = PinsService as unknown as {
  getApiV1Pins: ReturnType<typeof vi.fn>;
  getApiV1SessionsIdPins: ReturnType<typeof vi.fn>;
  postApiV1SessionsIdMessagesMessageidPin: ReturnType<typeof vi.fn>;
  deleteApiV1SessionsIdMessagesMessageidPin: ReturnType<typeof vi.fn>;
};

const PIN_ALPHA = { id: 1, session_id: "s1", message_id: 10, ordinal: 1, content: "alpha pin", role: "user", created_at: "", session_project: "alpha", session_title: "alpha session" };
const PIN_BETA  = { id: 2, session_id: "s2", message_id: 20, ordinal: 1, content: "beta pin",  role: "user", created_at: "", session_project: "beta",  session_title: "beta session"  };

describe("PinsStore.loadAll project filtering", () => {
  let store: ReturnType<typeof createPinsStore>;

  beforeEach(() => {
    store = createPinsStore();
    apiRuntimeMocks.callGenerated.mockReset();
    apiRuntimeMocks.callGenerated.mockImplementation(
      (request: () => Promise<unknown>, _signal?: AbortSignal) => request(),
    );
    pinsService.getApiV1Pins.mockResolvedValue({ pins: [] });
  });

  it("aborts an obsolete all-pins read when the project changes", async () => {
    const signals: AbortSignal[] = [];
    apiRuntimeMocks.callGenerated.mockImplementation((request, signal) => {
      signals.push(signal as AbortSignal);
      return request();
    });
    pinsService.getApiV1Pins
      .mockImplementationOnce(() => new Promise(() => {}))
      .mockResolvedValueOnce({ pins: [PIN_BETA] });

    void store.loadAll("alpha");
    await Promise.resolve();
    await store.loadAll("beta");

    expect(signals[0]?.aborted).toBe(true);
  });

  it("keeps all-pins and session-pins cancellation independent", async () => {
    const signals: AbortSignal[] = [];
    apiRuntimeMocks.callGenerated.mockImplementation((request, signal) => {
      signals.push(signal as AbortSignal);
      return request();
    });
    pinsService.getApiV1Pins.mockImplementationOnce(() => new Promise(() => {}));
    pinsService.getApiV1SessionsIdPins.mockImplementationOnce(
      () => new Promise(() => {}),
    );

    void store.loadAll();
    void store.loadForSession("s1");
    await Promise.resolve();
    store.cancelSessionPinsRead();

    expect(signals[0]?.aborted).toBe(false);
    expect(signals[1]?.aborted).toBe(true);
  });

  it("populates pins on successful load", async () => {
    pinsService.getApiV1Pins.mockResolvedValue({ pins: [PIN_ALPHA] });
    await store.loadAll("alpha");
    expect(store.pins).toEqual([PIN_ALPHA]);
  });

  it("clears pins immediately when project changes before fetch resolves", async () => {
    // Load project alpha successfully first.
    pinsService.getApiV1Pins.mockResolvedValue({ pins: [PIN_ALPHA] });
    await store.loadAll("alpha");
    expect(store.pins).toEqual([PIN_ALPHA]);

    // Switch to project beta — the fetch hangs; capture the in-flight call.
    let resolveBeta!: (v: { pins: typeof PIN_BETA[] }) => void;
    pinsService.getApiV1Pins.mockReturnValue(
      new Promise((r) => { resolveBeta = r; })
    );
    const betaLoad = store.loadAll("beta");

    // Before beta resolves, pins must already be empty.
    expect(store.pins).toHaveLength(0);

    // Resolve the beta fetch normally.
    resolveBeta({ pins: [PIN_BETA] });
    await betaLoad;
    expect(store.pins).toEqual([PIN_BETA]);
  });

  it("keeps pins empty after a failed load when project changes (regression)", async () => {
    // Load project alpha successfully.
    pinsService.getApiV1Pins.mockResolvedValue({ pins: [PIN_ALPHA] });
    await store.loadAll("alpha");
    expect(store.pins).toEqual([PIN_ALPHA]);

    // Switch to beta — the fetch fails.
    pinsService.getApiV1Pins.mockRejectedValue(new Error("network error"));
    await store.loadAll("beta");

    // Must not fall back to alpha's pins.
    expect(store.pins).toHaveLength(0);
    expect(store.loading).toBe(false);
  });

  it("preserves stale pins during re-fetch for the same project", async () => {
    pinsService.getApiV1Pins.mockResolvedValue({ pins: [PIN_ALPHA] });
    await store.loadAll("alpha");

    // Re-fetch the same project — fetch hangs.
    let resolve!: (v: { pins: typeof PIN_ALPHA[] }) => void;
    pinsService.getApiV1Pins.mockReturnValue(
      new Promise((r) => { resolve = r; })
    );
    const refetch = store.loadAll("alpha");

    // Pins must still be visible while the same-project refresh is in-flight.
    expect(store.pins).toEqual([PIN_ALPHA]);

    resolve({ pins: [PIN_ALPHA] });
    await refetch;
  });

  it("shows correct project pins after failed then successful project switch", async () => {
    // Load alpha.
    pinsService.getApiV1Pins.mockResolvedValue({ pins: [PIN_ALPHA] });
    await store.loadAll("alpha");

    // Switch to beta — fails.
    pinsService.getApiV1Pins.mockRejectedValue(new Error("network error"));
    await store.loadAll("beta");
    expect(store.pins).toHaveLength(0);

    // Switch back to alpha — succeeds.
    pinsService.getApiV1Pins.mockResolvedValue({ pins: [PIN_ALPHA] });
    await store.loadAll("alpha");
    expect(store.pins).toEqual([PIN_ALPHA]);
  });

  it("does not apply a superseded load response after project changes", async () => {
    // Start a slow alpha load.
    let resolveAlpha!: (v: { pins: typeof PIN_ALPHA[] }) => void;
    pinsService.getApiV1Pins.mockReturnValueOnce(
      new Promise((r) => { resolveAlpha = r; })
    );
    const alphaLoad = store.loadAll("alpha");

    // Before alpha resolves, switch to beta (fast, succeeds).
    pinsService.getApiV1Pins.mockResolvedValue({ pins: [PIN_BETA] });
    await store.loadAll("beta");
    expect(store.pins).toEqual([PIN_BETA]);

    // Now the stale alpha response arrives — must be discarded.
    resolveAlpha({ pins: [PIN_ALPHA] });
    await alphaLoad;
    expect(store.pins).toEqual([PIN_BETA]);
  });
});
