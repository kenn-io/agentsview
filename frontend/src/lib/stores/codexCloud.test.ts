import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { CodexcloudHomeStatus } from "../api/generated/index";
import { setServerUrl } from "../api/runtime.js";

const getStatus = vi.hoisted(() => vi.fn());

vi.mock("../api/generated/index", () => ({
  CodexCloudService: { getApiV1CodexCloudStatus: getStatus },
}));

const REMOTE = "http://remote.example:8080";
const LOCAL_ROWS: CodexcloudHomeStatus[] = [{ key: "local", homes: [], state: "ok" }];

// A fresh module per test, since the store is a singleton. ApiError comes
// from the same fresh module graph so the store recognizes it.
async function freshStore() {
  vi.resetModules();
  const { codexCloudStatus } = await import("./codexCloud.svelte.js");
  const { ApiError } = await import("../api/runtime.js");
  return { store: codexCloudStatus, ApiError };
}

describe("codexCloudStatus", () => {
  beforeEach(() => {
    getStatus.mockReset();
    setServerUrl("");
  });

  afterEach(() => {
    setServerUrl("");
  });

  it("loads status again after switching away from a server without the route", async () => {
    const { store, ApiError } = await freshStore();
    setServerUrl(REMOTE);
    getStatus.mockRejectedValueOnce(new ApiError(404, "not found"));
    await store.load();
    await store.load();
    expect(getStatus).toHaveBeenCalledTimes(1);

    setServerUrl("");
    getStatus.mockResolvedValueOnce(LOCAL_ROWS);
    await store.load();
    expect(store.rows).toEqual(LOCAL_ROWS);
  });

  it("drops a response for a request started under another server", async () => {
    const { store } = await freshStore();
    setServerUrl(REMOTE);
    let answer!: (rows: CodexcloudHomeStatus[]) => void;
    getStatus.mockReturnValueOnce(
      new Promise<CodexcloudHomeStatus[]>((resolve) => {
        answer = resolve;
      }),
    );
    const pending = store.load();
    setServerUrl("");
    answer([{ key: "remote", homes: [], state: "error" }]);
    await pending;
    expect(store.rows).toEqual([]);
  });
});
