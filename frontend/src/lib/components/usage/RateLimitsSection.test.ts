import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, render, screen, waitFor } from "@testing-library/svelte";
import { UsageService } from "../../api/generated/index";
import RateLimitsSection from "./RateLimitsSection.svelte";

vi.mock("../../api/generated/index", () => ({
  UsageService: { getApiV1RateLimits: vi.fn() },
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.resetAllMocks();
});

it("filters sparkline dates without changing current limits, then refreshes machine scope", async () => {
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(256);
  vi.spyOn(Date, "now").mockReturnValue(Date.parse("2024-01-01T10:10:00Z"));
  const current = {
    observed_at: "2024-01-01T10:10:00Z", limit_id: "codex",
    primary: { used_percent: 30, window_minutes: 300, resets_at: 1704110400 },
    credits: { balance: "12.50", unlimited: false, has_credits: true },
  };
  const item = {
    machine: "machine-a",
    limit_id: "codex",
    current,
    points: [
      {
        observed_at: "2023-12-31T10:00:00Z",
        limit_id: "codex",
        primary: { used_percent: 10, window_minutes: 300, resets_at: null },
      },
      current,
    ],
  };
  vi.mocked(UsageService.getApiV1RateLimits).mockResolvedValueOnce([item]);
  const view = render(RateLimitsSection, { machine: "machine-a", from: "2023-12-31", to: "2024-01-01", refreshKey: 0 });
  expect(await screen.findByText("30%")).toBeTruthy();
  expect(screen.getByText("Session limit")).toBeTruthy();
  expect(screen.getByText("5h")).toBeTruthy();
  expect(screen.getByText("Resets in 1h 50m")).toBeTruthy();
  expect(screen.getByText(/Credits:.*12.5/)).toBeTruthy();
  expect(screen.getByRole("progressbar").getAttribute("aria-valuenow")).toBe("30");
  await waitFor(() => expect(screen.getByRole("img").querySelector("path")?.getAttribute("d"))
    .toBe("M0,61.2L252,47.6"));
  vi.mocked(UsageService.getApiV1RateLimits).mockResolvedValueOnce([{ ...item, points: [] }]);
  await view.rerender({ from: "2024-01-02", to: "2024-01-02" });
  expect(await screen.findByText("30%")).toBeTruthy();
  expect(screen.queryByRole("img")).toBeNull();
  vi.mocked(UsageService.getApiV1RateLimits).mockRejectedValueOnce(new Error("offline"));
  await view.rerender({ machine: "machine-a", refreshKey: 1 });
  expect((await screen.findByRole("alert")).textContent).toBe("Could not load rate limits.");
  vi.mocked(UsageService.getApiV1RateLimits).mockResolvedValueOnce([]);
  await view.rerender({ machine: "machine-b", refreshKey: 1 });
  await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  expect(screen.queryByRole("region")).toBeNull();
  expect(UsageService.getApiV1RateLimits).toHaveBeenLastCalledWith(
    { machine: "machine-b", since: Date.parse("2024-01-02T00:00:00") / 1000, until: Date.parse("2024-01-03T00:00:00") / 1000 },
    { signal: expect.any(AbortSignal) },
  );
});
