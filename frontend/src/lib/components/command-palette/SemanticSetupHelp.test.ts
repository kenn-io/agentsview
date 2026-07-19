// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";
import { EmbeddingsService } from "../../api/generated/index.js";
import type { VectorBuildStatus } from "../../api/generated/index.js";
import { ApiError } from "../../api/runtime.js";

const { mockCopyToClipboard } = vi.hoisted(() => ({
  mockCopyToClipboard: vi.fn(),
}));

vi.mock("../../api/runtime.js", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/runtime.js")>();
  return {
    ...orig,
    callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  };
});

vi.mock("../../api/generated/index.js", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../../api/generated/index.js")>();
  return {
    ...orig,
    EmbeddingsService: {
      getApiV1EmbeddingsStatus: vi.fn(),
      postApiV1EmbeddingsBuild: vi.fn(),
    },
  };
});

vi.mock("../../utils/clipboard.js", () => ({
  copyToClipboard: mockCopyToClipboard,
}));

// @ts-ignore
import SemanticSetupHelp from "./SemanticSetupHelp.svelte";

const embeddingsService = EmbeddingsService as unknown as {
  getApiV1EmbeddingsStatus: ReturnType<typeof vi.fn>;
  postApiV1EmbeddingsBuild: ReturnType<typeof vi.fn>;
};

function idleStatus(overrides: Partial<VectorBuildStatus> = {}): VectorBuildStatus {
  return {
    running: false,
    done: 0,
    total: 0,
    eta_milliseconds: 0,
    ...overrides,
  };
}

function runningStatus(
  done: number,
  total: number,
  overrides: Partial<VectorBuildStatus> = {},
): VectorBuildStatus {
  return {
    running: true,
    phase: "embedding",
    done,
    total,
    eta_milliseconds: 0,
    ...overrides,
  };
}

describe("SemanticSetupHelp", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    embeddingsService.getApiV1EmbeddingsStatus.mockReset();
    embeddingsService.postApiV1EmbeddingsBuild.mockReset();
    mockCopyToClipboard.mockReset();
    mockCopyToClipboard.mockResolvedValue(true);
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

  function text(): string {
    return document.body.textContent ?? "";
  }

  function mountHelp(onResolved = vi.fn(), searchDetail: string | null = null) {
    const component = mount(SemanticSetupHelp, {
      target: document.body,
      props: { onResolved, searchDetail },
    });
    return { component, onResolved };
  }

  it("shows the config walkthrough when the daemon has no embeddings manager", async () => {
    embeddingsService.getApiV1EmbeddingsStatus.mockRejectedValue(
      new ApiError(501, "embeddings manager not available"),
    );

    const { component } = mountHelp();
    await settle();

    expect(text()).toContain("Semantic search isn't set up");
    expect(text()).toContain("[vector]");
    expect(text()).toContain("enabled = true");
    expect(text()).toContain("agentsview embeddings build");

    const copyButtons = document.body.querySelectorAll("button.kit-copy-btn");
    expect(copyButtons.length).toBe(2);
    (copyButtons[0] as HTMLButtonElement).click();
    await settle();
    expect(mockCopyToClipboard).toHaveBeenCalledWith(
      expect.stringContaining("[vector.embeddings.servers.local]"),
    );
    (copyButtons[1] as HTMLButtonElement).click();
    await settle();
    expect(mockCopyToClipboard).toHaveBeenCalledWith(
      "agentsview embeddings build",
    );

    unmount(component);
  });

  it("shows a specific unavailable reason verbatim instead of the walkthrough", async () => {
    const reason =
      "vector serving is disabled for this daemon run: another process held " +
      "vectors.write.lock at startup; wait for it to finish, then restart the daemon";
    embeddingsService.getApiV1EmbeddingsStatus.mockRejectedValue(
      new ApiError(501, reason),
    );

    const { component } = mountHelp();
    await settle();

    expect(text()).toContain("Semantic search is disabled");
    expect(text()).toContain(reason);
    expect(text()).not.toContain("enabled = true");

    unmount(component);
  });

  it.each([
    [
      "no matching generation",
      "semantic search not available: semantic search: PG has no embedding " +
        "generation matching fingerprint abc123 (present: def456); run " +
        "'agentsview pg push' from a machine with a matching " +
        "[vector.embeddings] config",
    ],
    [
      "missing generation chunk table",
      "semantic search not available: semantic search: PG generation 7 " +
        "matches fingerprint abc123 but its chunk table is missing " +
        "(interrupted push?); re-run 'agentsview pg push' from a machine " +
        "with a matching [vector.embeddings] config",
    ],
  ])(
    "shows the PostgreSQL %s reason when the status probe is generically unavailable",
    async (_name, reason) => {
      embeddingsService.getApiV1EmbeddingsStatus.mockRejectedValue(
        new ApiError(501, "embeddings manager not available"),
      );

      const { component } = mountHelp(vi.fn(), reason);
      await settle();

      expect(text()).toContain("Semantic search is disabled");
      expect(text()).toContain(reason);
      expect(text()).not.toContain("enabled = true");
      expect(text()).not.toContain("agentsview embeddings build");

      unmount(component);
    },
  );

  it("offers a build button when configured but not built, and resolves after the build", async () => {
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValueOnce(idleStatus());

    const { component, onResolved } = mountHelp();
    await settle();

    expect(text()).toContain("Semantic index not built yet");
    const build = [...document.body.querySelectorAll("button")].find((b) =>
      b.textContent?.includes("Build embeddings"),
    );
    expect(build).toBeDefined();

    embeddingsService.postApiV1EmbeddingsBuild.mockResolvedValueOnce({
      started: true,
    });
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValueOnce(
      runningStatus(500, 1000),
    );
    build!.click();
    await settle();
    expect(embeddingsService.postApiV1EmbeddingsBuild).toHaveBeenCalledWith({
      requestBody: {},
    });
    expect(text()).toContain("Building embeddings index...");

    await settle(2000);
    expect(text()).toContain("50%");

    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValueOnce(
      idleStatus({ last_result: undefined }),
    );
    await settle(2000);
    expect(onResolved).toHaveBeenCalledOnce();

    unmount(component);
  });

  it("shows a specific search 501 reason verbatim in the ready state", async () => {
    const stale =
      "semantic search not available: index is stale (embedding config " +
      "changed): run 'agentsview embeddings build --full-rebuild'";
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValueOnce(idleStatus());

    const { component } = mountHelp(vi.fn(), stale);
    await settle();

    expect(text()).toContain("Semantic index not built yet");
    expect(text()).toContain(stale);
    expect(text()).not.toContain(
      "Semantic search is configured, but the embeddings index hasn't been built",
    );

    unmount(component);
  });

  it("replaces the generic search 501 message with localized ready copy", async () => {
    const generic =
      "semantic search not available: enable [vector] in config.toml and " +
      "run 'agentsview embeddings build'";
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValueOnce(idleStatus());

    const { component } = mountHelp(vi.fn(), generic);
    await settle();

    expect(text()).toContain(
      "Semantic search is configured, but the embeddings index hasn't been built",
    );
    expect(text()).not.toContain("enable [vector] in config.toml");

    unmount(component);
  });

  it("surfaces a build failure returned by the initial status probe", async () => {
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValueOnce(
      idleStatus({ last_error: "background build failed" }),
    );

    const { component, onResolved } = mountHelp();
    await settle();

    expect(text()).toContain("Embeddings build failed");
    expect(text()).toContain("background build failed");
    expect(text()).not.toContain("Build embeddings");
    expect(onResolved).not.toHaveBeenCalled();

    unmount(component);
  });

  it("retries the search when the initial status probe sees a completed build", async () => {
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValueOnce(
      idleStatus({
        last_result: {
          Fingerprint: "fp-1",
          Activated: true,
          Refresh: { Upserted: 1, Deleted: 0, Unchanged: 0 },
          Repair: {
            scanned: false,
            scan_complete: false,
            documents: 0,
            chunks: 0,
            failed: 0,
            remaining: 0,
            remaining_known: false,
          },
          Fill: { Documents: 1, Chunks: 2, Skipped: 0, Stale: 0 },
        },
      }),
    );

    const { component, onResolved } = mountHelp();
    await settle();

    expect(onResolved).toHaveBeenCalledOnce();
    expect(text()).not.toContain("Build embeddings");

    unmount(component);
  });

  it("watches an already-running build instead of failing on 409", async () => {
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValueOnce(idleStatus());

    const { component, onResolved } = mountHelp();
    await settle();

    embeddingsService.postApiV1EmbeddingsBuild.mockRejectedValueOnce(
      new ApiError(409, "an embeddings build is already running"),
    );
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValueOnce(
      idleStatus(),
    );
    const build = [...document.body.querySelectorAll("button")].find((b) =>
      b.textContent?.includes("Build embeddings"),
    );
    build!.click();
    await settle();
    expect(text()).toContain("Building embeddings index...");

    await settle(2000);
    expect(onResolved).toHaveBeenCalledOnce();

    unmount(component);
  });

  it("jumps straight to progress when a build is already running at mount", async () => {
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValueOnce(
      runningStatus(250, 1000),
    );

    const { component } = mountHelp();
    await settle();

    expect(text()).toContain("Building embeddings index...");
    expect(text()).toContain("25%");

    unmount(component);
  });

  it("surfaces the build's failure message when the build ends in error", async () => {
    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValueOnce(
      runningStatus(0, 0, { phase: "scanning" }),
    );

    const { component, onResolved } = mountHelp();
    await settle();
    expect(text()).toContain("Scanning the archive...");

    embeddingsService.getApiV1EmbeddingsStatus.mockResolvedValueOnce(
      idleStatus({ last_error: "embeddings endpoint refused the connection" }),
    );
    await settle(2000);

    expect(text()).toContain("Embeddings build failed");
    expect(text()).toContain("embeddings endpoint refused the connection");
    expect(onResolved).not.toHaveBeenCalled();

    unmount(component);
  });
});
