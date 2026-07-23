import {
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { fetchRecallEntries } from "./recall.js";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("fetchRecallEntries", () => {
  it("sends every corpus-browser filter to the Recall API", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(
      JSON.stringify({ entries: [], trusted_only: false }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    ));
    vi.stubGlobal("fetch", fetchMock);

    await fetchRecallEntries({
      query: "bounded pass",
      project: "project-a",
      type: "decision",
      sourceRunId: "generation-a",
      reviewState: "human_reviewed",
      limit: 75,
    });

    expect(fetchMock).toHaveBeenCalledOnce();
    const url = new URL(String(fetchMock.mock.calls[0]?.[0]),
      window.location.origin);
    expect(url.pathname).toBe("/api/v1/recall/entries");
    expect(Object.fromEntries(url.searchParams)).toEqual({
      limit: "75",
      q: "bounded pass",
      project: "project-a",
      type: "decision",
      source_run_id: "generation-a",
      review_state: "human_reviewed",
    });
  });
});
