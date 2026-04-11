import { describe, it, expect, beforeEach, vi } from "vitest";
import * as api from "../api/client.js";
import { createPinsStore } from "./pins.svelte.js";

vi.mock("../api/client.js", () => ({
  listPins: vi.fn().mockResolvedValue({ pins: [] }),
  listSessionPins: vi.fn().mockResolvedValue({ message_ids: [] }),
  pinMessage: vi.fn().mockResolvedValue(undefined),
  unpinMessage: vi.fn().mockResolvedValue(undefined),
}));

const PIN_ALPHA = { id: 1, session_id: "s1", message_id: 10, ordinal: 1, content: "alpha pin", role: "user", created_at: "", session_project: "alpha", session_title: "alpha session" };
const PIN_BETA  = { id: 2, session_id: "s2", message_id: 20, ordinal: 1, content: "beta pin",  role: "user", created_at: "", session_project: "beta",  session_title: "beta session"  };

describe("PinsStore.loadAll project filtering", () => {
  let store: ReturnType<typeof createPinsStore>;

  beforeEach(() => {
    store = createPinsStore();
    vi.mocked(api.listPins).mockResolvedValue({ pins: [] });
  });

  it("populates pins on successful load", async () => {
    vi.mocked(api.listPins).mockResolvedValue({ pins: [PIN_ALPHA] });
    await store.loadAll("alpha");
    expect(store.pins).toEqual([PIN_ALPHA]);
  });

  it("clears pins immediately when project changes before fetch resolves", async () => {
    // Load project alpha successfully first.
    vi.mocked(api.listPins).mockResolvedValue({ pins: [PIN_ALPHA] });
    await store.loadAll("alpha");
    expect(store.pins).toEqual([PIN_ALPHA]);

    // Switch to project beta — the fetch hangs; capture the in-flight call.
    let resolveBeta!: (v: { pins: typeof PIN_BETA[] }) => void;
    vi.mocked(api.listPins).mockReturnValue(
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
    vi.mocked(api.listPins).mockResolvedValue({ pins: [PIN_ALPHA] });
    await store.loadAll("alpha");
    expect(store.pins).toEqual([PIN_ALPHA]);

    // Switch to beta — the fetch fails.
    vi.mocked(api.listPins).mockRejectedValue(new Error("network error"));
    await store.loadAll("beta");

    // Must not fall back to alpha's pins.
    expect(store.pins).toHaveLength(0);
    expect(store.loading).toBe(false);
  });

  it("preserves stale pins during re-fetch for the same project", async () => {
    vi.mocked(api.listPins).mockResolvedValue({ pins: [PIN_ALPHA] });
    await store.loadAll("alpha");

    // Re-fetch the same project — fetch hangs.
    let resolve!: (v: { pins: typeof PIN_ALPHA[] }) => void;
    vi.mocked(api.listPins).mockReturnValue(
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
    vi.mocked(api.listPins).mockResolvedValue({ pins: [PIN_ALPHA] });
    await store.loadAll("alpha");

    // Switch to beta — fails.
    vi.mocked(api.listPins).mockRejectedValue(new Error("network error"));
    await store.loadAll("beta");
    expect(store.pins).toHaveLength(0);

    // Switch back to alpha — succeeds.
    vi.mocked(api.listPins).mockResolvedValue({ pins: [PIN_ALPHA] });
    await store.loadAll("alpha");
    expect(store.pins).toEqual([PIN_ALPHA]);
  });

  it("does not apply a superseded load response after project changes", async () => {
    // Start a slow alpha load.
    let resolveAlpha!: (v: { pins: typeof PIN_ALPHA[] }) => void;
    vi.mocked(api.listPins).mockReturnValueOnce(
      new Promise((r) => { resolveAlpha = r; })
    );
    const alphaLoad = store.loadAll("alpha");

    // Before alpha resolves, switch to beta (fast, succeeds).
    vi.mocked(api.listPins).mockResolvedValue({ pins: [PIN_BETA] });
    await store.loadAll("beta");
    expect(store.pins).toEqual([PIN_BETA]);

    // Now the stale alpha response arrives — must be discarded.
    resolveAlpha({ pins: [PIN_ALPHA] });
    await alphaLoad;
    expect(store.pins).toEqual([PIN_BETA]);
  });
});
