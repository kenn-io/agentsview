import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { trends } from "./trends.svelte.js";
import { selectionFromRange } from "../components/shared/rangeSelection.js";
import { TrendsService } from "../api/generated/index";
import type { TrendsTermsResponse } from "../api/types.js";

// Capture the store's shipped default range at import time, before the
// beforeEach reset overwrites it.
const DEFAULT_RANGE = { from: trends.from, to: trends.to };

vi.mock("../api/runtime.js", () => ({
  configureGeneratedClient: vi.fn(),
  callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
}));

vi.mock("../api/generated/index", () => ({
  TrendsService: {
    getApiV1TrendsTerms: vi.fn(),
  },
}));

const trendsService = TrendsService as unknown as {
  getApiV1TrendsTerms: ReturnType<typeof vi.fn>;
};

function makeResponse(): TrendsTermsResponse {
  return {
    granularity: "week",
    from: "2024-01-01",
    to: "2024-01-31",
    message_count: 0,
    buckets: [],
    series: [],
  };
}

function resetStore() {
  trends.from = "2024-01-01";
  trends.to = "2024-01-31";
  trends.granularity = "week";
  trends.normalized = false;
  trends.termText = "load bearing | load-bearing\nseam";
  trends.response = null;
  trends.loading.terms = false;
  trends.errors.terms = null;
}

beforeEach(() => {
  resetStore();
  vi.clearAllMocks();
  trendsService.getApiV1TrendsTerms.mockResolvedValue(makeResponse());
});

describe("TrendsStore.fetchTerms", () => {
  it("fetches default terms with timezone and date range", async () => {
    await trends.fetchTerms();

    expect(trendsService.getApiV1TrendsTerms).toHaveBeenCalledWith(
      expect.objectContaining({
        from: "2024-01-01",
        to: "2024-01-31",
        granularity: "week",
        term: ["load bearing | load-bearing", "seam"],
        timezone: expect.any(String),
      }),
    );
    expect(trends.response?.granularity).toBe("week");
  });

  it("removes blank term lines", async () => {
    trends.termText = "seam\n\n  \nblast radius";

    await trends.fetchTerms();

    expect(trendsService.getApiV1TrendsTerms).toHaveBeenCalledWith(
      expect.objectContaining({
        term: ["seam", "blast radius"],
      }),
    );
  });

  it("sets first-load error state", async () => {
    trendsService.getApiV1TrendsTerms.mockRejectedValue(new Error("boom"));

    await trends.fetchTerms();

    expect(trends.response).toBeNull();
    expect(trends.loading.terms).toBe(false);
    expect(trends.errors.terms).toBe("boom");
  });

  it("keeps existing response and surfaces refetch errors", async () => {
    const existing = makeResponse();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    trends.response = existing;
    trendsService.getApiV1TrendsTerms.mockRejectedValue(new Error("boom"));

    await trends.fetchTerms();

    expect(trends.response).toEqual(existing);
    expect(trends.loading.terms).toBe(false);
    expect(trends.errors.terms).toBe("boom");
    expect(warn).toHaveBeenCalledWith(
      "trends.terms refetch failed:",
      expect.any(Error),
    );
    warn.mockRestore();
  });

  it("setGranularity refetches with the new granularity", async () => {
    await trends.setGranularity("month");

    expect(trendsService.getApiV1TrendsTerms).toHaveBeenCalledWith(
      expect.objectContaining({ granularity: "month" }),
    );
  });
});

describe("TrendsStore default range", () => {
  it("ships a 1y rolling default recognized as the relative preset", () => {
    // Under the shared N-days-inclusive semantics, the default must resolve
    // to the 365-day relative preset, not a 366-day custom range — otherwise
    // TrendsPage's selectionFromRange() would show the default as custom.
    const selection = selectionFromRange(
      DEFAULT_RANGE.from,
      DEFAULT_RANGE.to,
    );
    expect(selection).toEqual({ mode: "relative", days: 365 });
  });

  it("spans 365 calendar days inclusive of the end date", () => {
    const from = new Date(`${DEFAULT_RANGE.from}T00:00:00`);
    const to = new Date(`${DEFAULT_RANGE.to}T00:00:00`);
    const dayMs = 24 * 60 * 60 * 1000;
    const spanDays = Math.round((to.getTime() - from.getTime()) / dayMs) + 1;
    expect(spanDays).toBe(365);
  });
});
