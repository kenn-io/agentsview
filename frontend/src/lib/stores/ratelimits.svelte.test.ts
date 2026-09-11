import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { ServiceRateLimitWindow } from "../api/generated/index";
import type { RateLimitCardIdentity } from "./ratelimits.svelte.js";

const rateLimitsServiceMocks = vi.hoisted(() => ({
  getApiV1RateLimitsCurrent: vi.fn(),
  getApiV1RateLimitsHistory: vi.fn(),
}));

vi.mock("../api/runtime.js", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../api/runtime.js")>();
  return {
    ...orig,
    callGenerated: vi.fn((request: (o?: { signal?: AbortSignal }) => Promise<unknown>) => request()),
  };
});
vi.mock("../api/generated/index", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../api/generated/index")>();
  return { ...orig, RateLimitsService: rateLimitsServiceMocks };
});
vi.mock("./sessions.svelte.js", () => ({ sessions: { filters: { machine: "", agent: "" } } }));

// identity builds a full RateLimitCardIdentity fixture; overrides pick
// which axis differs between two cards under test.
function identity(overrides: Partial<RateLimitCardIdentity> = {}): RateLimitCardIdentity {
  return {
    vendor: "codex", accountId: "", machine: "laptop", limitId: "codex",
    windowKind: "primary", ...overrides,
  };
}
function snapshot(overrides: Partial<ServiceRateLimitWindow> = {}): ServiceRateLimitWindow {
  return {
    vendor: "codex", machine: "laptop", limitId: "codex", planType: "pro", windowKind: "primary",
    usedPercent: 42, windowMinutes: 10080, resetsAt: 1789435448, creditsHas: true,
    creditsUnlimited: false, creditsBalance: "100.0", observedAt: "2026-09-09T10:00:00Z", ...overrides,
  };
}

let rateLimits: (typeof import("./ratelimits.svelte.js"))["rateLimits"];

beforeEach(async () => {
  vi.clearAllMocks();
  ({ rateLimits } = await import("./ratelimits.svelte.js"));
  rateLimits.history = {};
});

// fetchHistory keys both its cache and its in-flight abort by the card's
// full identity: a request for one identity must not cancel or clobber
// another's, and a failed refetch for an identity already cached must
// leave that identity's prior history in place.
describe("rateLimits store", () => {
  it("dedups history requests per identity without cross-identity interference", async () => {
    let resolveLaptop: ((v: ServiceRateLimitWindow[]) => void) | undefined;
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockImplementationOnce(
      () => new Promise((resolve) => { resolveLaptop = resolve; }),
    );
    const laptopPromise = rateLimits.fetchHistory(identity({ machine: "laptop" }), "2026-09-08T00:00:00Z", "2026-09-09T23:59:59Z");
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValueOnce([snapshot({ machine: "desktop" })]);
    await rateLimits.fetchHistory(identity({ machine: "desktop" }), "2026-09-08T00:00:00Z", "2026-09-09T23:59:59Z");
    resolveLaptop?.([snapshot({ machine: "laptop" })]);
    await laptopPromise;
    expect(rateLimits.historyFor(identity({ machine: "laptop" }))).toHaveLength(1);
    expect(rateLimits.historyFor(identity({ machine: "desktop" }))).toHaveLength(1);

    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockRejectedValueOnce(new Error("boom"));
    await rateLimits.fetchHistory(identity({ machine: "laptop" }), "2026-09-08T00:00:00Z", "2026-09-09T23:59:59Z");
    expect(rateLimits.historyFor(identity({ machine: "laptop" }))).toHaveLength(1);
  });

  // A stale in-flight request for the same identity can still resolve
  // after a newer one already landed (abort does not guarantee the
  // underlying promise never settles); the newer request's result must
  // win regardless of resolution order.
  it("ignores a same-identity response that resolves after a newer request for it", async () => {
    let resolveFirst: ((v: ServiceRateLimitWindow[]) => void) | undefined;
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockImplementationOnce(
      () => new Promise((resolve) => { resolveFirst = resolve; }),
    );
    const firstPromise = rateLimits.fetchHistory(identity(), "2026-09-08T00:00:00Z", "2026-09-09T23:59:59Z");
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValueOnce([snapshot({ usedPercent: 99 })]);
    await rateLimits.fetchHistory(identity(), "2026-09-08T00:00:00Z", "2026-09-09T23:59:59Z");
    resolveFirst?.([snapshot({ usedPercent: 1 })]);
    await firstPromise;
    expect(rateLimits.historyFor(identity())).toEqual([snapshot({ usedPercent: 99 })]);
  });

  // Codex reports plan_type as a label that can flip between "pro" and
  // empty for the same window from one observation to the next, not a
  // stable identity component (see RateLimitCardIdentity), so
  // fetchHistory must keep every row for an identity regardless of each
  // row's own plan_type rather than dropping the ones that disagree.
  it("keeps every row for an identity regardless of differing plan_type", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValueOnce([
      snapshot({ planType: "pro" }),
      snapshot({ planType: "" }),
    ]);
    await rateLimits.fetchHistory(identity(), "2026-09-08T00:00:00Z", "2026-09-09T23:59:59Z");
    expect(rateLimits.historyFor(identity())).toHaveLength(2);
  });
});
