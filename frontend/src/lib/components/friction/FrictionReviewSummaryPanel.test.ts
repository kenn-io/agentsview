// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, render, screen } from "@testing-library/svelte";
import { tick } from "svelte";

const mocks = vi.hoisted(() => ({
  generateInsight: vi.fn(),
  getInsights: vi.fn(),
  setAgent: vi.fn(),
  agent: "claude",
  requestAgent: undefined as string | undefined,
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
    get requestAgent() {
      return mocks.requestAgent;
    },
  },
}));

import FrictionReviewSummaryPanel from "./FrictionReviewSummaryPanel.svelte";

function saved(date: string, content: string, revision = 1) {
  return {
    provenance_json: JSON.stringify({
      source_versions: { friction_digest_revision: String(revision) },
    }),
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
  for (const fn of [mocks.generateInsight, mocks.getInsights, mocks.setAgent]) fn.mockReset();
  mocks.serverVersion = { read_only: false };
  mocks.agent = "claude";
  mocks.requestAgent = undefined;
  mocks.getInsights.mockResolvedValue({ insights: [] });
});

describe("FrictionReviewSummaryPanel", () => {
  it("labels model output and describes its inputs and side effects", async () => {
    render(FrictionReviewSummaryPanel, { date: "2025-01-15", revision: 1 });
    await settle();
    expect(screen.getByText(/model-written summary/i)).toBeTruthy();
    expect(screen.getByText(/does not change findings, digests, or issues/i)).toBeTruthy();
  });

  it("lists saved insights for exactly the digest date", async () => {
    render(FrictionReviewSummaryPanel, { date: "2025-01-15", revision: 1 });
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
    render(FrictionReviewSummaryPanel, { date: "2025-01-15", revision: 1 });
    await settle();
    await settle();
    expect(screen.getByText("Bash failures dominate.")).toBeTruthy();
    expect(screen.getByRole("button", { name: /regenerate/i })).toBeTruthy();
  });

  it("hides a summary saved for an older revision of the digest", async () => {
    mocks.getInsights.mockResolvedValue({
      insights: [saved("2025-01-15", "Old summary.", 1)],
    });
    render(FrictionReviewSummaryPanel, { date: "2025-01-15", revision: 2 });
    await settle();
    await settle();
    expect(screen.queryByText("Old summary.")).toBeNull();
    expect(screen.getByRole("button", { name: /generate summary/i })).toBeTruthy();
  });

  it.each([
    ["shows a summary of the digest on screen", 1, true],
    ["hides a summary of a digest rebuilt during generation", 2, false],
  ])("%s", async (_name, generatedRevision, shown) => {
    mocks.generateInsight.mockReturnValue({
      abort: vi.fn(),
      done: Promise.resolve(saved("2025-01-15", "Fresh summary.", generatedRevision)),
    });
    render(FrictionReviewSummaryPanel, { date: "2025-01-15", revision: 1 });
    await settle();
    await fireEvent.click(screen.getByRole("button", { name: /generate summary/i }));
    await settle();
    await settle();
    expect(screen.queryByText("Fresh summary.") !== null).toBe(shown);
  });

  it("retries a failed read as a read, even when generation is unavailable", async () => {
    mocks.serverVersion = { read_only: true, insight_generation_available: false };
    mocks.getInsights
      .mockRejectedValueOnce(new Error("list failed"))
      .mockResolvedValueOnce({ insights: [saved("2025-01-15", "Saved summary.")] });
    render(FrictionReviewSummaryPanel, { date: "2025-01-15", revision: 1 });
    await settle();
    await settle();
    const retry = screen.getByRole("button", { name: /retry/i });
    expect((retry as HTMLButtonElement).disabled).toBe(false);
    await fireEvent.click(retry);
    await settle();
    await settle();
    expect(mocks.getInsights).toHaveBeenCalledTimes(2);
    expect(mocks.generateInsight).not.toHaveBeenCalled();
    expect(screen.getByText("Saved summary.")).toBeTruthy();
  });

  it("retries a failed regeneration as a regeneration", async () => {
    mocks.getInsights.mockResolvedValue({
      insights: [saved("2025-01-15", "Bash failures dominate.")],
    });
    mocks.generateInsight
      .mockReturnValueOnce({ abort: vi.fn(), done: Promise.reject(new Error("model unavailable")) })
      .mockReturnValueOnce({ abort: vi.fn(), done: new Promise(() => {}) });
    render(FrictionReviewSummaryPanel, { date: "2025-01-15", revision: 1 });
    await settle();
    await settle();
    await fireEvent.click(screen.getByRole("button", { name: /regenerate/i }));
    await settle();
    await fireEvent.click(screen.getByRole("button", { name: /retry/i }));
    expect(mocks.generateInsight).toHaveBeenCalledTimes(2);
    expect(mocks.generateInsight.mock.calls[1]?.[0]).toMatchObject({ force_refresh: true });
  });

  it.each([
    {
      name: "leaves the agent to the server until one is chosen",
      requestAgent: undefined,
      want: {},
    },
    { name: "sends the chosen agent", requestAgent: "codex", want: { agent: "codex" } },
  ])("explicitly opts in and $name", async ({ requestAgent, want }) => {
    mocks.requestAgent = requestAgent;
    mocks.generateInsight.mockReturnValue({ abort: vi.fn(), done: new Promise(() => {}) });
    render(FrictionReviewSummaryPanel, { date: "2025-01-15", revision: 1 });
    await settle();
    await fireEvent.click(screen.getByRole("button", { name: /generate summary/i }));
    expect(mocks.generateInsight).toHaveBeenCalledWith(
      {
        type: "llm_canned",
        kind: "friction_review",
        llm_opt_in: true,
        date_from: "2025-01-15",
        date_to: "2025-01-15",
        ...want,
      },
      expect.any(Function),
    );
  });

  it("disables generation when the archive cannot generate insights", async () => {
    mocks.serverVersion = { read_only: true, insight_generation_available: false };
    render(FrictionReviewSummaryPanel, { date: "2025-01-15", revision: 1 });
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
    const { rerender } = render(FrictionReviewSummaryPanel, { date: "2025-01-15", revision: 1 });
    await settle();
    await fireEvent.click(screen.getByRole("button", { name: /generate summary/i }));
    await rerender({ date: "2025-01-14", revision: 1 });
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
    const { rerender } = render(FrictionReviewSummaryPanel, { date: "2025-01-15", revision: 1 });
    await settle();
    await settle();
    expect(screen.getByText("OLD SUMMARY")).toBeTruthy();

    await rerender({ date: "2025-01-14", revision: 1 });
    await settle();
    expect(screen.queryByText("OLD SUMMARY")).toBeNull();
    expect(screen.queryByText("test-model")).toBeNull();
  });
});
