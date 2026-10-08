// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import ToolUsage from "./ToolUsage.svelte";
import { AnalyticsService, type DbSignalSessionsResponse } from "../../api/generated/index.js";
import { router } from "../../stores/router.svelte.js";
import { analytics } from "../../stores/analytics.svelte.js";

const missingRates = {
  analyzed_calls: 0,
  known_outcome_calls: 0,
  empty_calls: 0,
  repeated_calls: 0,
  recovered_sequences: 0,
  abandoned_sequences: 0,
  open_sequences: 0,
  unknown_sequences: 0,
  missing_calls: 0,
  empty_rate: null,
  repeat_rate: null,
  recovery_rate: null,
};

describe("ToolUsage", () => {
  afterEach(() => {
    analytics.tools = null;
    // @ts-ignore
    analytics.errors = {
      ...analytics.errors,
      tools: null,
    };
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  it("renders ranked per-tool analysis rows", async () => {
    analytics.tools = {
      total_calls: 6,
      by_category: [
        { category: "Read", count: 3, pct: 50 },
        { category: "Bash", count: 2, pct: 33.3 },
      ],
      by_agent: [],
      by_tool: [
        {
          ...missingRates,
          tool_name: "Read",
          category: "Read",
          call_count: 3,
          session_count: 2,
          pct: 50,
        },
        {
          ...missingRates,
          tool_name: "Bash",
          category: "Bash",
          call_count: 2,
          session_count: 1,
          pct: 33.3,
        },
      ],
      trend: [
        {
          date: "2024-06-03",
          by_category: { Read: 3, Bash: 2 },
        },
        {
          date: "2024-06-10",
          by_category: { Read: 1 },
        },
      ],
    };

    const component = mount(ToolUsage, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Tool Usage");
    expect(document.body.textContent).toContain("6 calls");
    expect(document.body.textContent).toContain("Top tools");
    expect(document.body.textContent).toContain("Read");
    expect(document.body.textContent).toContain("3");
    expect(document.body.textContent).toContain("2 sessions");
    expect(document.body.textContent).toContain("50%");
    expect(document.body.textContent).toContain("Bash");
    expect(document.body.textContent).toContain("1 session");
    expect(document.body.textContent).not.toContain("1 sessions");
    expect(document.body.textContent).toContain("33.3%");
    expect(document.body.textContent).toContain("By Category");
    expect(document.body.textContent).toContain("Weekly Trend");

    unmount(component);
  });

  it("renders loading state while the first fetch is in flight", async () => {
    analytics.tools = null;
    analytics.loading = { ...analytics.loading, tools: true };

    const component = mount(ToolUsage, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Loading tool usage...");
    expect(document.body.textContent).not.toContain("No tool usage data");

    analytics.loading = { ...analytics.loading, tools: false };
    unmount(component);
  });

  it("renders empty state without tool rows", async () => {
    analytics.tools = {
      total_calls: 0,
      by_category: [],
      by_agent: [],
      by_tool: [],
      trend: [],
    };

    const component = mount(ToolUsage, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("No tool usage data");

    unmount(component);
  });
  function seedRates() {
    analytics.loading = { ...analytics.loading, tools: false };
    analytics.tools = {
      total_calls: 7,
      by_category: [{ category: "Grep", count: 7, pct: 100 }],
      by_agent: [],
      trend: [],
      by_tool: [
        {
          ...missingRates,
          tool_name: "Grep",
          category: "Grep",
          call_count: 12,
          session_count: 4,
          pct: 100,
          analyzed_calls: 11,
          known_outcome_calls: 10,
          empty_calls: 7,
          repeated_calls: 5,
          recovered_sequences: 2,
          abandoned_sequences: 1,
          open_sequences: 1,
          unknown_sequences: 1,
          missing_calls: 1,
          empty_rate: 0.7,
          repeat_rate: 5 / 11,
          recovery_rate: 2 / 3,
        },
      ],
    };
  }

  it("shows partial coverage and each rate's denominator", async () => {
    seedRates();
    const component = mount(ToolUsage, { target: document.body });
    await tick();
    expect(document.body.textContent).toContain("11/12");
    expect(document.body.textContent).toContain("agentsview sync --full");
    expect(document.body.textContent).toContain("70.0%");
    expect(document.body.textContent).toContain("45.5%");
    expect(document.body.textContent).toContain("66.7%");
    expect(
      document.querySelector('[title="7/10 empty results among known outcomes."]'),
    ).not.toBeNull();
    expect(
      document.querySelector('[title="2/3 recovered sequences. Open: 1. Unknown: 1."]'),
    ).not.toBeNull();
    analytics.tools!.by_tool![0]!.repeat_rate = 0;
    await tick();
    expect(document.body.textContent).toContain("0.0%");
    expect(document.querySelector('[aria-label="Grep Repeat rate"]')).toBeNull();
    expect(document.querySelector('[aria-label="Grep Empty rate"]')).not.toBeNull();
    unmount(component);
  });

  it("opens contributing sessions with the active filters and ignores an older response", async () => {
    seedRates();
    analytics.project = "project-a";
    analytics.model = "model-b";
    let resolveOlder!: (value: DbSignalSessionsResponse) => void;
    const older = new Promise<DbSignalSessionsResponse>((resolve) => (resolveOlder = resolve));
    const example = {
      session_id: "recovered",
      project: "project-a",
      agent: "claude",
      date: "2025-06-02",
      is_automated: false,
      outcome: "",
      health_score: null,
      health_grade: null,
      signal_total: 1,
      reason_code: "tool_recovery_rate",
      excerpt: "",
      message_ordinal: 2,
      failure_signals: 0,
      retries: 0,
      edit_churn: 0,
    };
    const fetch = vi
      .spyOn(AnalyticsService, "getApiV1AnalyticsSignalSessions")
      .mockReturnValueOnce(older)
      .mockResolvedValue({ signal: "tool_recovery_rate", total: 1, sessions: [example] });
    const navigate = vi.spyOn(router, "navigateToSession").mockImplementation(() => {});
    const component = mount(ToolUsage, { target: document.body });
    await tick();
    (document.querySelector('[aria-label="Grep Empty rate"]') as HTMLButtonElement).click();
    await tick();
    (document.querySelector('[aria-label="Grep Recovery rate"]') as HTMLButtonElement).click();
    await tick();
    await tick();
    expect(fetch).toHaveBeenLastCalledWith(
      expect.objectContaining({
        project: "project-a",
        model: "model-b",
        tool_name: "Grep",
        signal: "tool_recovery_rate",
      }),
      expect.anything(),
    );
    expect(fetch.mock.calls[0]?.[1]?.signal?.aborted).toBe(true);
    resolveOlder({
      signal: "tool_empty_rate",
      total: 1,
      sessions: [{ ...example, session_id: "stale", project: "stale-project" }],
    });
    await tick();
    await tick();
    expect(document.body.textContent).not.toContain("stale-project");
    expect(document.body.textContent).toContain("Matching calls: 1");
    expect(document.body.textContent).toContain("Latest matching call: 2025-06-02");
    (document.querySelector("a.evidence-row") as HTMLAnchorElement).click();
    expect(navigate).toHaveBeenCalledWith("recovered", { msg: "2" });
    const row = analytics.tools!.by_tool[0]!;
    analytics.tools!.by_tool = [
      { ...row, tool_name: "readFile", category: "Read" },
      { ...row, tool_name: "readFile", category: "MCP" },
    ];
    fetch.mockResolvedValue({ signal: "tool_empty_rate", total: 2, next_offset: 1, sessions: [] });
    await tick();
    const buttons = document.querySelectorAll('[aria-label="readFile Empty rate"]');
    (buttons[0] as HTMLButtonElement).click();
    await tick();
    await tick();
    expect(fetch).toHaveBeenLastCalledWith(
      expect.objectContaining({ tool_name: "readFile", tool_category: "Read", offset: 0 }),
      expect.anything(),
    );
    const more = [...document.querySelectorAll("button")].find((button) =>
      button.textContent?.includes("More sessions"),
    )!;
    more.click();
    await tick();
    await tick();
    expect(fetch).toHaveBeenLastCalledWith(
      expect.objectContaining({ tool_category: "Read", offset: 1 }),
      expect.anything(),
    );
    fetch.mockResolvedValueOnce({ signal: "tool_empty_rate", total: 0, sessions: [] })
      .mockResolvedValue({ signal: "tool_empty_rate", sessions: [] });
    (buttons[1] as HTMLButtonElement).click();
    await tick();
    await tick();
    expect(document.querySelector(".tool-evidence")?.textContent).toContain("0 sessions");
    analytics.project = "project-b";
    await tick();
    await tick();
    expect(fetch).toHaveBeenLastCalledWith(
      expect.objectContaining({ tool_category: "MCP", offset: 0 }),
      expect.anything(),
    );
    expect(document.querySelector(".tool-evidence")?.textContent).not.toContain("0 sessions");
    unmount(component);
    analytics.project = "";
    analytics.model = "";
  });
});
