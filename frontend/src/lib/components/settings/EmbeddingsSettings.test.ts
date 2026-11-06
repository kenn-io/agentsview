// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { flushSync, mount, unmount } from "svelte";
// @ts-ignore
import EmbeddingsSettings from "./EmbeddingsSettings.svelte";
// @ts-ignore
import { EmbeddingsService } from "../../api/generated/index";
import type { VectorBuildStatus } from "../../api/generated/index";
import { ApiError } from "../../api/runtime.js";

vi.mock("../../api/runtime.js", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/runtime.js")>();
  return {
    ...orig,
  };
});

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    EmbeddingsService: {
      getApiV1EmbeddingsStatus: vi.fn(),
      getApiV1EmbeddingsGenerations: vi.fn(),
      postApiV1EmbeddingsSetup: vi.fn(),
    },
  };
});

const embeddingsService = EmbeddingsService as unknown as {
  getApiV1EmbeddingsStatus: ReturnType<typeof vi.fn>;
  getApiV1EmbeddingsGenerations: ReturnType<typeof vi.fn>;
  postApiV1EmbeddingsSetup: ReturnType<typeof vi.fn>;
};

function runningStatus(
  done: number,
  overrides: Partial<VectorBuildStatus> = {},
): VectorBuildStatus {
  return {
    running: true,
    setup: {
      enabled: true,
      action: "",
      provider: "built-in",
      busy: false,
      installing: false,
      missing_bytes: 0,
      downloaded_bytes: 0,
      total_bytes: 0,
    },
    build_id: 1,
    started_at: "2026-07-11T10:00:00Z",
    phase: "embedding",
    done,
    total: 1000,
    eta_milliseconds: 0,
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
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-11T10:00:05Z"));
    embeddingsService.getApiV1EmbeddingsStatus.mockReset();
    embeddingsService.getApiV1EmbeddingsGenerations.mockReset();
    embeddingsService.postApiV1EmbeddingsSetup.mockReset();
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

  it("approves only the disclosed download when retrying after Ollama stops", async () => {
    const setup = {
      enabled: false,
      action: "turn-on",
      provider: "ollama",
      missing_bytes: 295000000,
      busy: false,
      installing: false,
      downloaded_bytes: 0,
      total_bytes: 0,
    };
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValue({ running: false, setup });
    embeddingsService.postApiV1EmbeddingsSetup.mockResolvedValue({ started: true });
    const component = mount(EmbeddingsSettings, { target: document.body });
    try {
      await settle();
      expect(document.body.textContent).toContain("Uses Ollama");
      expect(document.body.textContent).not.toContain("Downloads 295 MB");
      embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValue({
        running: false,
        setup: {
          ...setup,
          provider: "built-in",
          error: "embedding download declined; approve installation to use local embeddings",
        },
      });
      [...document.body.querySelectorAll("button")]
        .find((b) => b.textContent?.trim() === "Turn on")!
        .click();
      await settle();
      expect(embeddingsService.postApiV1EmbeddingsSetup).toHaveBeenNthCalledWith(1, {
        approved_bytes: 0,
      });
      expect(document.body.textContent).toContain("Downloads 295 MB");
      expect(document.body.querySelector('[role="alert"]')?.textContent).toContain(
        "download declined",
      );
      [...document.body.querySelectorAll("button")]
        .find((b) => b.textContent?.trim() === "Retry")!
        .click();
      await settle();
      expect(embeddingsService.postApiV1EmbeddingsSetup).toHaveBeenNthCalledWith(2, {
        approved_bytes: 295000000,
      });
    } finally {
      unmount(component);
    }
  });

  it("retries Turn on after setup fails", async () => {
    const setup = {
      enabled: true,
      action: "turn-on",
      provider: "built-in",
      error: "Another process may be using search. Close it and retry.",
      busy: false,
      installing: false,
      missing_bytes: 0,
      downloaded_bytes: 0,
      total_bytes: 0,
    };
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValue({ running: false, setup });
    embeddingsService.postApiV1EmbeddingsSetup.mockResolvedValue({ started: true });
    const component = mount(EmbeddingsSettings, { target: document.body });
    await settle();
    expect(document.body.textContent).toContain(setup.error);
    const button = [...document.body.querySelectorAll("button")].find(
      (b) => b.textContent?.trim() === "Retry",
    );
    expect(button).toBeTruthy();
    expect(document.body.querySelectorAll("button")).toHaveLength(1);
    button!.click();
    await settle();
    expect(embeddingsService.postApiV1EmbeddingsSetup).toHaveBeenCalledWith({ approved_bytes: 0 });
    unmount(component);
  });

  it("follows a concurrent build after Turn on returns 409 without retaining an error", async () => {
    const setup = {
      enabled: false,
      action: "turn-on",
      provider: "built-in",
      busy: false,
      installing: false,
      missing_bytes: 0,
      downloaded_bytes: 0,
      total_bytes: 0,
    };
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValue({ running: false, setup });
    embeddingsService.postApiV1EmbeddingsSetup.mockImplementation(async () => {
      embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValue(
        runningStatus(1, { setup: { ...setup, enabled: true, action: "" } }),
      );
      throw new ApiError(409, "embedding build already running");
    });
    const component = mount(EmbeddingsSettings, { target: document.body });
    await settle();
    [...document.body.querySelectorAll("button")]
      .find((b) => b.textContent?.trim() === "Turn on")!
      .click();
    await settle();
    expect(document.body.textContent).toContain("Indexing");
    expect(document.body.textContent).not.toContain("embedding build already running");
    expect(document.body.querySelector('[role="alert"]')).toBeNull();
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValue(
      runningStatus(2, { setup: { ...setup, enabled: true, action: "" } }),
    );
    await settle(2000);
    expect(document.body.textContent).toContain("Indexing 2 of");
    unmount(component);
  });

  it("shows indexing progress with throughput and remaining time", async () => {
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValue(
      runningStatus(200, {
        estimate_ready: true,
        rate_per_second: 50,
        eta_milliseconds: 16_000,
        setup: {
          enabled: true,
          action: "",
          provider: "built-in",
          busy: false,
          installing: false,
          missing_bytes: 0,
          downloaded_bytes: 0,
          total_bytes: 0,
        },
      }),
    );

    const component = mount(EmbeddingsSettings, { target: document.body });
    await settle();

    const text = () => document.body.textContent ?? "";
    expect(text()).toContain("test-embed-model");
    expect(text()).toContain("256");
    expect(text()).toContain("Indexing 200 of 1,000 text sections");
    expect(text()).toContain("under a minute left");
    expect(text()).toContain("50 text sections/s");
    expect(text()).toContain("Elapsed 5s");

    expect(document.body.querySelectorAll('[role="progressbar"]')).toHaveLength(1);
    const progressbar = document.body.querySelector('[role="progressbar"]');
    expect(progressbar?.getAttribute("aria-valuenow")).toBe("200");
    expect(progressbar?.getAttribute("aria-valuemax")).toBe("1000");

    unmount(component);
  });

  it("shows indexing while the daemon warms its estimate", async () => {
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValue(
      runningStatus(100, { estimate_ready: false, eta_milliseconds: 16_000 }),
    );

    const component = mount(EmbeddingsSettings, { target: document.body });
    try {
      await settle();
      expect(document.body.textContent).toContain("Indexing 100 of 1,000 text sections");
      expect(document.body.textContent).not.toContain("under a minute left");
    } finally {
      unmount(component);
    }
  });

  it("shows the scanning state without inventing totals", async () => {
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValue(
      runningStatus(0, { phase: "scanning", total: 0 }),
    );

    const component = mount(EmbeddingsSettings, { target: document.body });
    await settle();

    expect(document.body.textContent).toContain("Scanning");
    expect(document.body.textContent).toContain("the total amount of work is not known yet");
    expect(document.body.querySelector('[role="progressbar"]')?.getAttribute("aria-valuemax")).toBe(
      "0",
    );

    unmount(component);
  });

  it("switches to the completed summary and refetches generations when the build finishes", async () => {
    let current: VectorBuildStatus = runningStatus(900);
    embeddingsService.getApiV1EmbeddingsStatus.mockImplementation(() => Promise.resolve(current));

    const component = mount(EmbeddingsSettings, { target: document.body });
    await settle();
    const generationCallsWhileRunning =
      embeddingsService.getApiV1EmbeddingsGenerations.mock.calls.length;

    current = {
      setup: runningStatus(0).setup,
      running: false,
      build_id: 1,
      started_at: "2026-07-11T10:00:00Z",
      done: 1000,
      total: 1000,
      eta_milliseconds: 0,
      model: "test-embed-model",
      dimension: 256,
      last_result: {
        Fingerprint: "fp-1",
        Activated: true,
        Refresh: { Upserted: 5, Deleted: 0, Unchanged: 100 },
        Repair: {
          scanned: false,
          scan_complete: false,
          documents: 0,
          chunks: 0,
          failed: 0,
          remaining: 0,
          remaining_known: false,
        },
        Fill: { Documents: 12, Chunks: 345, Skipped: 0, Stale: 0 },
      },
    };
    embeddingsService.getApiV1EmbeddingsGenerations.mockResolvedValue({
      generations: [{ ...BUILDING_GENERATION, state: "active", embedded: 100, missing: 0 }],
    });
    await settle(2000);

    const text = document.body.textContent ?? "";
    expect(text).toContain("Completed");
    expect(text).toContain("new generation activated");
    expect(text).toContain("345");
    expect(text).toContain("Active");
    expect(text).toContain("100 embedded");
    expect(embeddingsService.getApiV1EmbeddingsGenerations.mock.calls.length).toBeGreaterThan(
      generationCallsWhileRunning,
    );

    unmount(component);
  });

  it("shows the last build error when idle after a failure", async () => {
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValue({
      running: false,
      done: 0,
      total: 0,
      eta_milliseconds: 0,
      model: "test-embed-model",
      dimension: 256,
      setup: runningStatus(0).setup,
      last_error: "encode batch: connection refused",
    });

    const component = mount(EmbeddingsSettings, { target: document.body });
    await settle();

    expect(document.body.textContent).toContain("encode batch: connection refused");

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
    expect(document.body.textContent).toContain("vector serving disabled: vectors.write.lock held");

    const calls = embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length;
    await settle(10 * 60_000);
    expect(embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length).toBe(calls);

    unmount(component);
  });

  it("polls fast while running, slowly while idle, and not at all when hidden or unmounted", async () => {
    let current: VectorBuildStatus = runningStatus(100);
    embeddingsService.getApiV1EmbeddingsStatus.mockImplementation(() => Promise.resolve(current));

    const component = mount(EmbeddingsSettings, { target: document.body });
    await settle();
    const afterMount = embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length;
    expect(afterMount).toBe(1);

    // Active cadence: a running build is polled every 2s.
    await settle(2000);
    expect(embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length).toBe(2);

    // Idle cadence: after the build finishes, 2s ticks stop...
    current = {
      setup: runningStatus(0).setup,
      running: false,
      done: 0,
      total: 0,
      eta_milliseconds: 0,
      model: "test-embed-model",
      dimension: 256,
    };
    await settle(2000);
    const idleBase = embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length;
    await settle(10_000);
    expect(embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length).toBe(idleBase);
    // ...but a slow poll still notices externally started builds.
    await settle(60_000);
    expect(embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length).toBeGreaterThan(idleBase);

    // Hidden page: polling stops entirely.
    Object.defineProperty(document, "hidden", {
      configurable: true,
      get: () => true,
    });
    document.dispatchEvent(new Event("visibilitychange"));
    const hiddenBase = embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length;
    await settle(10 * 60_000);
    expect(embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length).toBe(hiddenBase);

    // Visible again: an immediate refresh resumes the loop.
    Object.defineProperty(document, "hidden", {
      configurable: true,
      get: () => false,
    });
    document.dispatchEvent(new Event("visibilitychange"));
    await settle();
    expect(embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length).toBeGreaterThan(
      hiddenBase,
    );

    // Unmount: no further polling.
    unmount(component);
    const afterUnmount = embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length;
    await settle(10 * 60_000);
    expect(embeddingsService.getApiV1EmbeddingsStatus.mock.calls.length).toBe(afterUnmount);
  });
});
