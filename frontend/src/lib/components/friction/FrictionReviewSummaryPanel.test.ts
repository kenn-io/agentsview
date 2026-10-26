// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, render, screen } from "@testing-library/svelte";
import { tick } from "svelte";

const mocks = vi.hoisted(() => ({
  generateInsight: vi.fn(),
  getInsights: vi.fn(),
  setAgent: vi.fn(),
  agent: "claude",
  serverVersion: { read_only: false } as {
    read_only: boolean;
    insight_generation_available?: boolean;
  } | null,
}));
vi.mock("../../api/generated/index", () => ({
  InsightsService: { getApiV1Insights: mocks.getInsights },
}));
vi.mock("../../api/runtime.js", () => ({ isAbortError: vi.fn(() => false) }));
vi.mock("../../api/client.js", () => ({ generateInsight: mocks.generateInsight }));
vi.mock("../../stores/sync.svelte.js", () => ({
  sync: {
    get serverVersion() {
      return mocks.serverVersion;
    },
  },
}));
vi.mock("../../stores/insights.svelte.js", () => ({
  insights: {
    setAgent: mocks.setAgent,
    get agent() {
      return mocks.agent;
    },
  },
}));

import FrictionReviewSummaryPanel from "./FrictionReviewSummaryPanel.svelte";

class ResizeObserverMock {
  observe = vi.fn();
  disconnect = vi.fn();
}

function saved(date: string, content: string) {
  return {
    id: 7,
    type: "llm_canned",
    kind: "friction_review",
    date_from: date,
    date_to: date,
    project: null,
    agent: "claude",
    model: "test-model",
    prompt: null,
    content,
    created_at: "2025-01-16T00:00:00Z",
  };
}

function settle() {
  return Promise.resolve().then(() => tick());
}

beforeEach(() => {
  Object.defineProperty(globalThis, "ResizeObserver", {
    configurable: true,
    writable: true,
    value: ResizeObserverMock,
  });
  for (const fn of [mocks.generateInsight, mocks.getInsights, mocks.setAgent]) fn.mockReset();
  mocks.serverVersion = { read_only: false };
  mocks.agent = "claude";
  mocks.getInsights.mockResolvedValue({ insights: [] });
});

describe("FrictionReviewSummaryPanel", () => {
  it("labels model output and describes its inputs and side effects", async () => {
    render(FrictionReviewSummaryPanel, { date: "2025-01-15" });
    await settle();
    expect(screen.getByText(/model-written summary/i)).toBeTruthy();
    expect(screen.getByText(/does not change findings, digests, or issues/i)).toBeTruthy();
  });

  it("lists saved insights for exactly the digest date", async () => {
    render(FrictionReviewSummaryPanel, { date: "2025-01-15" });
    await settle();
    expect(mocks.getInsights.mock.lastCall?.[0]).toEqual({
      type: "llm_canned",
      date_from: "2025-01-15",
      date_to: "2025-01-15",
    });
  });

  it("renders saved output with a regenerate action", async () => {
    mocks.getInsights.mockResolvedValue({
      insights: [saved("2025-01-15", "Bash failures dominate.")],
    });
    render(FrictionReviewSummaryPanel, { date: "2025-01-15" });
    await settle();
    await settle();
    expect(screen.getByText("Bash failures dominate.")).toBeTruthy();
    expect(screen.getByRole("button", { name: /regenerate/i })).toBeTruthy();
  });

  it("explicitly opts in with the selected agent", async () => {
    mocks.agent = "codex";
    mocks.generateInsight.mockReturnValue({ abort: vi.fn(), done: new Promise(() => {}) });
    render(FrictionReviewSummaryPanel, { date: "2025-01-15" });
    await settle();
    await fireEvent.click(screen.getByRole("button", { name: /generate summary/i }));
    expect(mocks.generateInsight).toHaveBeenCalledWith(
      {
        type: "llm_canned",
        kind: "friction_review",
        llm_opt_in: true,
        date_from: "2025-01-15",
        date_to: "2025-01-15",
        agent: "codex",
      },
      expect.any(Function),
    );
  });

  it("disables generation when the archive cannot generate insights", async () => {
    mocks.serverVersion = { read_only: true, insight_generation_available: false };
    render(FrictionReviewSummaryPanel, { date: "2025-01-15" });
    await settle();
    const button = screen.getByRole("button", { name: /generate summary/i }) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(button.title).toMatch(/not available/i);
  });

  it("aborts and ignores a generation that settles after the date changed", async () => {
    let resolveStale!: (value: unknown) => void;
    const abortStale = vi.fn();
    mocks.generateInsight.mockReturnValueOnce({
      abort: abortStale,
      done: new Promise((resolve) => {
        resolveStale = resolve;
      }),
    });
    const { rerender } = render(FrictionReviewSummaryPanel, { date: "2025-01-15" });
    await settle();
    await fireEvent.click(screen.getByRole("button", { name: /generate summary/i }));
    await rerender({ date: "2025-01-14" });
    await settle();
    expect(abortStale).toHaveBeenCalled();
    resolveStale(saved("2025-01-15", "STALE SUMMARY"));
    await settle();
    expect(screen.queryByText("STALE SUMMARY")).toBeNull();
  });

  it("hides a saved summary while loading another date", async () => {
    mocks.getInsights
      .mockResolvedValueOnce({ insights: [saved("2025-01-15", "OLD SUMMARY")] })
      .mockImplementationOnce(() => new Promise(() => {}));
    const { rerender } = render(FrictionReviewSummaryPanel, { date: "2025-01-15" });
    await settle();
    await settle();
    expect(screen.getByText("OLD SUMMARY")).toBeTruthy();

    await rerender({ date: "2025-01-14" });
    await settle();
    expect(screen.queryByText("OLD SUMMARY")).toBeNull();
    expect(screen.queryByText("test-model")).toBeNull();
  });
});
