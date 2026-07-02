// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vite-plus/test";
import { render, screen, fireEvent } from "@testing-library/svelte";
import { tick } from "svelte";

const mocks = vi.hoisted(() => ({
  generateInsight: vi.fn(),
  getInsights: vi.fn(),
  setType: vi.fn(),
  setDateFrom: vi.fn(),
  setDateTo: vi.fn(),
  setProject: vi.fn(),
  setAgent: vi.fn(),
  navigate: vi.fn(),
  agent: "claude" as string,
  serverVersion: {
    read_only: false,
  } as {
    read_only: boolean;
    insight_generation_available?: boolean;
  } | null,
}));
vi.mock("../../api/generated/index", () => ({
  InsightsService: { getApiV1Insights: mocks.getInsights },
}));
vi.mock("../../api/runtime.js", () => ({ configureGeneratedClient: vi.fn() }));
vi.mock("../../api/client.js", () => ({
  generateInsight: mocks.generateInsight,
}));
vi.mock("../../stores/sync.svelte.js", () => ({
  sync: {
    get serverVersion() {
      return mocks.serverVersion;
    },
  },
}));
vi.mock("../../stores/insights.svelte.js", () => ({
  insights: {
    setType: mocks.setType,
    setDateFrom: mocks.setDateFrom,
    setDateTo: mocks.setDateTo,
    setProject: mocks.setProject,
    setAgent: mocks.setAgent,
    get agent() {
      return mocks.agent;
    },
  },
}));
vi.mock("../../stores/router.svelte.js", () => ({
  router: { navigate: mocks.navigate },
  getBasePath: () => "",
}));

import ActivityInsight from "./ActivityInsight.svelte";

// jsdom has no ResizeObserver; the kit-ui Typeahead observes its open
// option list to keep the fixed-position popover placed correctly.
class ResizeObserverMock {
  observe = vi.fn();
  disconnect = vi.fn();
}

beforeEach(() => {
  Object.defineProperty(globalThis, "ResizeObserver", {
    configurable: true,
    writable: true,
    value: ResizeObserverMock,
  });
  for (const m of Object.values(mocks)) {
    if (typeof m === "function") m.mockReset();
  }
  mocks.serverVersion = { read_only: false };
  mocks.agent = "claude";
  mocks.getInsights.mockResolvedValue({ insights: [] });
});

function settle() {
  return Promise.resolve().then(() => tick());
}

describe("ActivityInsight", () => {
  it("fetches insights with both date bounds (full identity)", async () => {
    render(ActivityInsight, { dateFrom: "2026-06-15", dateTo: "2026-06-21" });
    await settle();
    expect(mocks.getInsights).toHaveBeenCalledWith({
      type: "daily_activity",
      dateFrom: "2026-06-15",
      dateTo: "2026-06-21",
    });
  });

  it("ignores project-scoped insights and shows the empty state", async () => {
    mocks.getInsights.mockResolvedValue({
      insights: [
        {
          id: 1,
          project: "p1",
          content: "scoped",
          type: "daily_activity",
          date_from: "2026-06-15",
          date_to: "2026-06-21",
          agent: "claude",
          model: null,
          prompt: null,
          created_at: "2026-06-21T00:00:00Z",
        },
      ],
    });
    render(ActivityInsight, { dateFrom: "2026-06-15", dateTo: "2026-06-21" });
    await settle();
    expect(screen.getByRole("button", { name: /generate/i })).toBeTruthy();
  });

  it("generates for the current range", async () => {
    mocks.generateInsight.mockReturnValue({
      abort: vi.fn(),
      done: new Promise(() => {}),
    });
    render(ActivityInsight, { dateFrom: "2026-06-15", dateTo: "2026-06-21" });
    await settle();
    await fireEvent.click(screen.getByRole("button", { name: /generate/i }));
    expect(mocks.generateInsight).toHaveBeenCalledWith(
      expect.objectContaining({
        type: "daily_activity",
        date_from: "2026-06-15",
        date_to: "2026-06-21",
        agent: "claude",
      }),
      expect.any(Function),
    );
  });

  it("lets the user choose the insight agent", async () => {
    render(ActivityInsight, { dateFrom: "2026-06-15", dateTo: "2026-06-21" });
    await settle();
    const trigger = screen.getByLabelText(/insight agent/i);
    expect(trigger.textContent).toContain("Claude");
    await fireEvent.click(trigger);
    await fireEvent.mouseDown(screen.getByRole("option", { name: "Codex" }));
    expect(mocks.setAgent).toHaveBeenCalledWith("codex");
  });

  it("generates with the selected agent, not a hardcoded one", async () => {
    mocks.agent = "codex";
    mocks.generateInsight.mockReturnValue({
      abort: vi.fn(),
      done: new Promise(() => {}),
    });
    render(ActivityInsight, { dateFrom: "2026-06-15", dateTo: "2026-06-21" });
    await settle();
    await fireEvent.click(screen.getByRole("button", { name: /generate/i }));
    expect(mocks.generateInsight).toHaveBeenCalledWith(
      expect.objectContaining({ agent: "codex" }),
      expect.any(Function),
    );
  });

  it("ignores a generation that settles after the range changed", async () => {
    let resolveStale!: (insight: unknown) => void;
    const abortStale = vi.fn();
    mocks.generateInsight.mockReturnValueOnce({
      abort: abortStale,
      done: new Promise((r) => {
        resolveStale = r;
      }),
    });
    const { rerender } = render(ActivityInsight, {
      dateFrom: "2026-06-15",
      dateTo: "2026-06-21",
    });
    await settle();

    // Start a generation for the first range.
    await fireEvent.click(screen.getByRole("button", { name: /generate/i }));
    expect(mocks.generateInsight).toHaveBeenCalledTimes(1);

    // Range change aborts and invalidates the in-flight generation.
    await rerender({ dateFrom: "2026-06-08", dateTo: "2026-06-14" });
    await settle();
    expect(abortStale).toHaveBeenCalled();

    // The aborted generation settles late; its result must not reach the panel.
    resolveStale({
      id: 99,
      project: null,
      content: "STALE RESULT",
      type: "daily_activity",
      date_from: "2026-06-15",
      date_to: "2026-06-21",
      agent: "claude",
      model: null,
      prompt: null,
      created_at: "2026-06-21T00:00:00Z",
    });
    await settle();

    expect(document.body.textContent).not.toContain("STALE RESULT");
  });

  it("prefills the Insights page range and navigates", async () => {
    render(ActivityInsight, { dateFrom: "2026-06-15", dateTo: "2026-06-21" });
    await settle();
    const link = screen.getByRole("link", {
      name: /insights page|open in insights/i,
    });
    await fireEvent.click(link);
    expect(mocks.setType).toHaveBeenCalledWith("daily_activity");
    expect(mocks.setDateFrom).toHaveBeenCalledWith("2026-06-15");
    expect(mocks.setDateTo).toHaveBeenCalledWith("2026-06-21");
    expect(mocks.setProject).toHaveBeenCalledWith("");
    expect(mocks.navigate).toHaveBeenCalledWith("insights");
  });

  it("disables Generate when the server version is unavailable", async () => {
    mocks.serverVersion = null;
    render(ActivityInsight, { dateFrom: "2026-06-15", dateTo: "2026-06-21" });
    await settle();
    const btn = screen.getByRole("button", { name: /generate/i });
    expect(btn.hasAttribute("disabled")).toBe(true);
  });

  it("allows Generate in read-only mode when insight generation is advertised", async () => {
    mocks.serverVersion = {
      read_only: true,
      insight_generation_available: true,
    };
    render(ActivityInsight, { dateFrom: "2026-06-15", dateTo: "2026-06-21" });
    await settle();
    const btn = screen.getByRole("button", { name: /generate/i });
    expect(btn.hasAttribute("disabled")).toBe(false);
  });

  it("selects the exact-range insight over a newer nested one", async () => {
    mocks.getInsights.mockResolvedValue({
      insights: [
        {
          id: 3,
          project: null,
          content: "SINGLE DAY",
          type: "daily_activity",
          date_from: "2026-06-15",
          date_to: "2026-06-15",
          agent: "claude",
          model: null,
          prompt: null,
          created_at: "2026-06-22T00:00:00Z",
        },
        {
          id: 2,
          project: null,
          content: "WEEKLY ROLLUP",
          type: "daily_activity",
          date_from: "2026-06-15",
          date_to: "2026-06-21",
          agent: "claude",
          model: null,
          prompt: null,
          created_at: "2026-06-21T00:00:00Z",
        },
      ],
    });
    render(ActivityInsight, { dateFrom: "2026-06-15", dateTo: "2026-06-21" });
    await settle();
    const text = document.body.textContent ?? "";
    expect(text).toContain("WEEKLY ROLLUP");
    expect(text).not.toContain("SINGLE DAY");
  });

  it("shows the model of the displayed insight", async () => {
    mocks.getInsights.mockResolvedValue({
      insights: [
        {
          id: 5,
          project: null,
          content: "summary",
          type: "daily_activity",
          date_from: "2026-06-15",
          date_to: "2026-06-21",
          agent: "claude",
          model: "claude-sonnet-4.5",
          prompt: null,
          created_at: "2026-06-21T00:00:00Z",
        },
      ],
    });
    render(ActivityInsight, { dateFrom: "2026-06-15", dateTo: "2026-06-21" });
    await settle();
    expect(document.body.textContent).toContain("claude-sonnet-4.5");
  });

  it("hides the model while a range change is loading", async () => {
    mocks.getInsights
      .mockResolvedValueOnce({
        insights: [
          {
            id: 9,
            project: null,
            content: "A",
            type: "daily_activity",
            date_from: "2026-06-01",
            date_to: "2026-06-07",
            agent: "claude",
            model: "model-A",
            prompt: null,
            created_at: "2026-06-07T00:00:00Z",
          },
        ],
      })
      .mockReturnValueOnce(new Promise(() => {})); // second range stays pending
    const { rerender } = render(ActivityInsight, {
      dateFrom: "2026-06-01",
      dateTo: "2026-06-07",
    });
    await settle();
    expect(document.body.textContent).toContain("model-A");
    await rerender({ dateFrom: "2026-06-08", dateTo: "2026-06-14" });
    await settle();
    // The new range is still loading, so the stale model must not show.
    expect(document.body.textContent).not.toContain("model-A");
    expect(document.body.textContent).toContain("Loading insight");
  });
});
