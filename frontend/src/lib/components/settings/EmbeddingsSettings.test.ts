// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";
// @ts-ignore
import EmbeddingsSettings from "./EmbeddingsSettings.svelte";
import { EmbeddingsService } from "../../api/generated/index";
import type { VectorBuildStatus } from "../../api/generated/index";
import { ApiError } from "../../api/runtime.js";

vi.mock("../../api/runtime.js", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/runtime.js")>();
  return {
    ...orig,
    callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  };
});

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    EmbeddingsService: {
      getApiV1EmbeddingsStatus: vi.fn(),
      getApiV1EmbeddingsGenerations: vi.fn(),
    },
  };
});

const embeddingsService = EmbeddingsService as unknown as {
  getApiV1EmbeddingsStatus: ReturnType<typeof vi.fn>;
  getApiV1EmbeddingsGenerations: ReturnType<typeof vi.fn>;
};

function runningStatus(
  done: number,
  overrides: Partial<VectorBuildStatus> = {},
): VectorBuildStatus {
  return {
    running: true,
    build_id: 1,
    started_at: "2026-07-11T10:00:00Z",
    phase: "embedding",
    done,
    total: 1000,
    model: "test-embed-model",
    dimension: 256,
    ...overrides,
  };
}

const BUILDING_GENERATION = {
  id: 1,
  state: "building",
  model: "test-embed-model",
  dimension: 256,
  fingerprint: "fp-1",
  embedded: 10,
  missing: 90,
};

describe("EmbeddingsSettings", () => {
  // The component samples progress with performance.now(); tie it to the
  // faked Date clock so vi.advanceTimersByTimeAsync moves both together.
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-11T10:00:05Z"));
    vi.spyOn(performance, "now").mockImplementation(() => Date.now());
    embeddingsService.getApiV1EmbeddingsStatus.mockReset();
    embeddingsService.getApiV1EmbeddingsGenerations.mockReset();
    embeddingsService.getApiV1EmbeddingsGenerations.mockResolvedValue({
      generations: [BUILDING_GENERATION],
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
    document.body.innerHTML = "";
  });

  async function settle(ms = 0): Promise<void> {
    await vi.advanceTimersByTimeAsync(ms);
    flushSync();
  }

  it("shows model, phase, progress, and an estimating state, then a smoothed ETA", async () => {
    let current: VectorBuildStatus = runningStatus(0);
    embeddingsService.getApiV1EmbeddingsStatus.mockImplementation(() =>
      Promise.resolve(current),
    );

    const component = mount(EmbeddingsSettings, { target: document.body });
    await settle();

    const text = () => document.body.textContent ?? "";
    expect(text()).toContain("test-embed-model");
    expect(text()).toContain("256");
    expect(text()).toContain("Embedding");
    expect(text()).toContain("0 / 1,000 chunks");
    expect(text()).toContain("Estimating time remaining...");

    // One positive delta is not enough for a stable rate.
    current = runningStatus(100);
    await settle(2000);
    expect(text()).toContain("100 / 1,000 chunks");
    expect(text()).toContain("10%");
    expect(text()).toContain("Estimating time remaining...");

    // Second delta at the same pace: 100 chunks / 2s -> 50 chunks/s, so the
    // remaining 800 chunks are 16 seconds away.
    current = runningStatus(200);
    await settle(2000);
    expect(text()).toContain("50 chunks/s");
    expect(text()).toContain("ETA 16s");
    expect(text()).toContain("Elapsed 9s");

    const progressbar = document.body.querySelector('[role="progressbar"]');
    expect(progressbar?.getAttribute("aria-valuenow")).toBe("200");
    expect(progressbar?.getAttribute("aria-valuemax")).toBe("1000");

    unmount(component);
  });

  it("resets the estimator when a different build takes over", async () => {
    let current: VectorBuildStatus = runningStatus(0);
    embeddingsService.getApiV1EmbeddingsStatus.mockImplementation(() =>
      Promise.resolve(current),
    );

    const component = mount(EmbeddingsSettings, { target: document.body });
    await settle();
    current = runningStatus(100);
    await settle(2000);
    current = runningStatus(200);
    await settle(2000);
    expect(document.body.textContent).toContain("ETA");
    expect(document.body.textContent).not.toContain(
      "Estimating time remaining...",
    );

    // A new build (fresh build_id) must not inherit the previous rate even
    // though progress keeps increasing monotonically.
    current = runningStatus(300, {
      build_id: 2,
      started_at: "2026-07-11T10:01:00Z",
    });
    await settle(2000);
    expect(document.body.textContent).toContain(
      "Estimating time remaining...",
    );

    unmount(component);
  });

  it("shows the scanning state without inventing totals", async () => {
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValue(
      runningStatus(0, { phase: "scanning", total: 0 }),
    );

    const component = mount(EmbeddingsSettings, { target: document.body });
    await settle();

    expect(document.body.textContent).toContain("Scanning");
    expect(document.body.textContent).toContain(
      "the total amount of work is not known yet",
    );
    expect(document.body.querySelector('[role="progressbar"]')).toBeNull();

    unmount(component);
  });

  it("switches to the completed summary and refetches generations when the build finishes", async () => {
    let current: VectorBuildStatus = runningStatus(900);
    embeddingsService.getApiV1EmbeddingsStatus.mockImplementation(() =>
      Promise.resolve(current),
    );

    const component = mount(EmbeddingsSettings, { target: document.body });
    await settle();
    const generationCallsWhileRunning =
      embeddingsService.getApiV1EmbeddingsGenerations.mock.calls.length;

    current = {
      running: false,
      build_id: 1,
      started_at: "2026-07-11T10:00:00Z",
      done: 1000,
      total: 1000,
      model: "test-embed-model",
      dimension: 256,
      last_result: {
        Fingerprint: "fp-1",
        Activated: true,
        Refresh: { Upserted: 5, Deleted: 0, Unchanged: 100 },
        Fill: { Documents: 12, Chunks: 345, Skipped: 0, Stale: 0 },
      },
    };
    embeddingsService.getApiV1EmbeddingsGenerations.mockResolvedValue({
      generations: [
        { ...BUILDING_GENERATION, state: "active", embedded: 100, missing: 0 },
      ],
    });
    await settle(2000);

    const text = document.body.textContent ?? "";
    expect(text).toContain("Completed");
    expect(text).toContain("new generation activated");
    expect(text).toContain("345");
    expect(text).toContain("Active");
    expect(text).toContain("100 embedded");
    expect(
      embeddingsService.getApiV1EmbeddingsGenerations.mock.calls.length,
    ).toBeGreaterThan(generationCallsWhileRunning);

    unmount(component);
  });

  it("shows the last build error when idle after a failure", async () => {
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValue({
      running: false,
      done: 0,
      total: 0,
      model: "test-embed-model",
      dimension: 256,
      last_error: "encode batch: connection refused",
    });

    const component = mount(EmbeddingsSettings, { target: document.body });
    await settle();

    expect(document.body.textContent).toContain("Failed");
    expect(document.body.textContent).toContain(
      "encode batch: connection refused",
    );

    unmount(component);
  });

  it("renders the unavailable state from a 501 and stops polling", async () => {
    embeddingsService.getApiV1EmbeddingsStatus.mockRejectedValue(
      new ApiError(501, "vector serving disabled: vectors.write.lock held"),
    );

    const component = mount(EmbeddingsSettings, { target: document.body });
    await settle();

    expect(document.body.textContent).toContain(
      "Semantic search embeddings are not available on this server.",
    );
    expect(document.body.textContent).toContain(
      "vector serving disabled: vectors.write.lock held",
    );

    const calls = embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length;
    await settle(10 * 60_000);
    expect(embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length).toBe(
      calls,
    );

    unmount(component);
  });

  it("polls fast while running, slowly while idle, and not at all when hidden or unmounted", async () => {
    let current: VectorBuildStatus = runningStatus(100);
    embeddingsService.getApiV1EmbeddingsStatus.mockImplementation(() =>
      Promise.resolve(current),
    );

    const component = mount(EmbeddingsSettings, { target: document.body });
    await settle();
    const afterMount =
      embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length;
    expect(afterMount).toBe(1);

    // Active cadence: a running build is polled every 2s.
    await settle(2000);
    expect(embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length).toBe(
      2,
    );

    // Idle cadence: after the build finishes, 2s ticks stop...
    current = {
      running: false,
      done: 0,
      total: 0,
      model: "test-embed-model",
      dimension: 256,
    };
    await settle(2000);
    const idleBase =
      embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length;
    await settle(10_000);
    expect(embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length).toBe(
      idleBase,
    );
    // ...but a slow poll still notices externally started builds.
    await settle(60_000);
    expect(
      embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length,
    ).toBeGreaterThan(idleBase);

    // Hidden page: polling stops entirely.
    Object.defineProperty(document, "hidden", {
      configurable: true,
      get: () => true,
    });
    document.dispatchEvent(new Event("visibilitychange"));
    const hiddenBase =
      embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length;
    await settle(10 * 60_000);
    expect(embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length).toBe(
      hiddenBase,
    );

    // Visible again: an immediate refresh resumes the loop.
    Object.defineProperty(document, "hidden", {
      configurable: true,
      get: () => false,
    });
    document.dispatchEvent(new Event("visibilitychange"));
    await settle();
    expect(
      embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length,
    ).toBeGreaterThan(hiddenBase);

    // Unmount: no further polling.
    unmount(component);
    const afterUnmount =
      embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length;
    await settle(10 * 60_000);
    expect(embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length).toBe(
      afterUnmount,
    );
  });
});
