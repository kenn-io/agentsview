import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { RecallService } from "./generated/index.js";
afterEach(() => {
  vi.unstubAllGlobals();
});

describe("generated recall entries", () => {
  it("sends every corpus-browser filter to the Recall API", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          entries: [],
          trusted_only: false,
          next_cursor: "cursor-2",
          result_cap: 500,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const page = await RecallService.getApiV1RecallEntries({
      q: "bounded pass",
      project: "project-a",
      type: "decision",
      source_run_id: "generation-a",
      review_state: "human_reviewed",
      limit: 75,
    });

    expect(fetchMock).toHaveBeenCalledOnce();
    const url = new URL(String(fetchMock.mock.calls[0]?.[0]), window.location.origin);
    expect(url.pathname).toBe("/api/v1/recall/entries");
    expect(Object.fromEntries(url.searchParams)).toEqual({
      limit: "75",
      q: "bounded pass",
      project: "project-a",
      type: "decision",
      source_run_id: "generation-a",
      review_state: "human_reviewed",
    });
    expect(page).toEqual({
      entries: [],
      trusted_only: false,
      next_cursor: "cursor-2",
      result_cap: 500,
    });
  });
});

describe("generated recall extraction progress", () => {
  it("sends bounded generation, state, and cursor filters", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          generation_fingerprint: "generation-a",
          progress: [],
          next_cursor: "progress-cursor-2",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const page = await RecallService.getApiV1RecallExtractionProgress({
      generation: "generation-a",
      state: "failed",
      cursor: "progress-cursor-1",
      limit: 25,
    });

    expect(fetchMock).toHaveBeenCalledOnce();
    const url = new URL(String(fetchMock.mock.calls[0]?.[0]), window.location.origin);
    expect(url.pathname).toBe("/api/v1/recall/extraction/progress");
    expect(Object.fromEntries(url.searchParams)).toEqual({
      limit: "25",
      generation: "generation-a",
      state: "failed",
      cursor: "progress-cursor-1",
    });
    expect(page).toEqual({
      generation_fingerprint: "generation-a",
      progress: [],
      next_cursor: "progress-cursor-2",
    });
  });
});

describe("Recall extraction generation actions", () => {
  it("activates the configured generation", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(null, {
        status: 204,
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await RecallService.postApiV1RecallExtractionActivate();

    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/recall/extraction/activate",
      expect.objectContaining({ method: "POST" }),
    );
  });

  it("retires one generation without a force option", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(null, {
        status: 204,
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await RecallService.postApiV1RecallExtractionGenerationsByFingerprintRetire({
      fingerprint: "generation old",
    });

    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/recall/extraction/generations/generation%20old/retire",
      expect.objectContaining({ method: "POST" }),
    );
  });
});

describe("generated recall import", () => {
  it("sends NDJSON records without JSON quoting", async () => {
    const ndjson = '{"candidate_id":"a"}\n{"candidate_id":"b"}\n';
    const body = new Blob([ndjson], { type: "application/x-ndjson" });
    const fetchMock = vi.fn().mockResolvedValue(
      new Response('{"imported":0,"skipped":0}', {
        headers: { "Content-Type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await RecallService.postApiV1RecallImport(body);

    expect(fetchMock).toHaveBeenCalledOnce();
    const init = fetchMock.mock.calls[0]?.[1];
    expect(init?.body).toBe(body);
    expect(await (init?.body as Blob).text()).toBe(ndjson);
  });
});
