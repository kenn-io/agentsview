import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { trends } from "./trends.svelte.js";
import { TrendsService } from "../api/generated/index";
import type { TrendsTermsResponse } from "../api/types.js";

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
