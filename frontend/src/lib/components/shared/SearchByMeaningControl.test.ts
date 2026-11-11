// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { flushSync, mount, unmount } from "svelte";
import SearchByMeaningControl from "./SearchByMeaningControl.svelte";
import { EmbeddingsService } from "../../api/generated/index.js";
import { ApiError } from "../../api/runtime.js";

vi.mock("../../api/generated/index.js", () => ({
  EmbeddingsService: { postApiV1EmbeddingsSetup: vi.fn() },
}));

afterEach(() => {
  document.body.innerHTML = "";
  vi.resetAllMocks();
});

it.each([
  ["embeddings_provider_changed", false],
  ["embeddings_build_running", true],
])("handles %s", async (code, started) => {
  vi.mocked(EmbeddingsService.postApiV1EmbeddingsSetup).mockRejectedValueOnce(
    new ApiError(409, "conflict", code),
  );
  const onchange = vi.fn();
  const component = mount(SearchByMeaningControl, {
    target: document.body,
    props: {
      status: {
        running: false,
        setup: {
          enabled: false,
          action: "turn-on",
          provider: "built-in",
          busy: false,
          installing: false,
          missing_bytes: 0,
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
  flushSync();
  unmount(component);
});
