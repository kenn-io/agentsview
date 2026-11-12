// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { flushSync, mount, unmount } from "svelte";
import SearchByMeaningControl from "./SearchByMeaningControl.svelte";
import { EmbeddingsService } from "../../api/generated/index.js";
import { ApiError } from "../../api/runtime.js";

vi.mock("../../api/generated/index.js", () => ({
  EmbeddingsService: { postApiV1EmbeddingsSetup: vi.fn(), postApiV1EmbeddingsBuild: vi.fn() },
}));

afterEach(() => {
  document.body.innerHTML = "";
  vi.resetAllMocks();
});

it.each([
  ["embeddings_provider_changed", false, "ollama", 0],
  ["conflict", true, "built-in", 295000000],
])("handles %s", async (code, started, provider, approvedBytes) => {
  vi.mocked(EmbeddingsService.postApiV1EmbeddingsSetup).mockRejectedValueOnce(
    new ApiError(409, "conflict", code),
  );
  const onchange = vi.fn();
  const component = mount(SearchByMeaningControl, {
    target: document.body,
    props: {
      status: {
        running: false,
        done: 0,
        eta_milliseconds: 0,
        total: 0,
        setup: {
          enabled: false,
          action: "turn-on",
          provider,
          busy: false,
          installing: false,
          missing_bytes: 295000000,
          downloaded_bytes: 0,
          total_bytes: 0,
        },
      },
      hint: true,
      onchange,
    },
  });
  document.querySelector<HTMLButtonElement>("button")!.click();
  await vi.waitFor(() => expect(onchange).toHaveBeenCalledWith(started));
  expect(EmbeddingsService.postApiV1EmbeddingsSetup).toHaveBeenCalledWith({
    approved_bytes: approvedBytes,
    expected_provider: provider,
  });
  flushSync();
  unmount(component);
});
