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
  ["embeddings_provider_changed", false, "ollama", 0, false],
  ["conflict", true, "built-in", 295000000, false],
  ["started", true, "ollama", 0, true],
])("handles %s", async (code, started, provider, approvedBytes, success) => {
  if (success) {
    vi.mocked(EmbeddingsService.postApiV1EmbeddingsSetup).mockResolvedValueOnce({ started: true });
  } else {
    vi.mocked(EmbeddingsService.postApiV1EmbeddingsSetup).mockRejectedValueOnce(
      new ApiError(409, "conflict", code),
    );
  }
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

it.each([""])("reports an unavailable provider on retry: %s", async (provider) => {
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
          error: "Previous setup failed",
          busy: false,
          installing: false,
          missing_bytes: 0,
          downloaded_bytes: 0,
          total_bytes: 0,
        },
      },
      hint: false,
      onchange: vi.fn(),
    },
  });
  const retry = [...document.querySelectorAll("button")].find((button) => button.textContent?.includes("Retry"));
  expect(retry).toBeDefined();
  retry!.click();
  await vi.waitFor(() => expect(document.querySelector('[role="alert"]')?.textContent).toBe("Failed to load embeddings status"));
  expect(EmbeddingsService.postApiV1EmbeddingsSetup).not.toHaveBeenCalled();
  unmount(component);
});
