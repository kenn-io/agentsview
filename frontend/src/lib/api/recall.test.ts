import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { RecallService } from "./generated/index.js";
afterEach(() => {
  vi.unstubAllGlobals();
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
  });
});
