// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import ToolUsage from "./ToolUsage.svelte";
import { AnalyticsService, type DbSignalSessionsResponse } from "../../api/generated/index.js";
import { router } from "../../stores/router.svelte.js";
import { analytics } from "../../stores/analytics.svelte.js";
import { getLocale } from "../../i18n/index.js";

vi.mock("../../i18n/index.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../i18n/index.js")>();
  return { ...actual, getLocale: vi.fn(() => "en") };
});

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
    vi.mocked(getLocale).mockImplementation(() => "en");
    analytics.project = "";
    analytics.model = "";
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

    await unmount(component);
  });

  it("renders loading state while the first fetch is in flight", async () => {
    analytics.tools = null;
    analytics.loading = { ...analytics.loading, tools: true };

    const component = mount(ToolUsage, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Loading tool usage...");
    expect(document.body.textContent).not.toContain("No tool usage data");

    analytics.loading = { ...analytics.loading, tools: false };
    await unmount(component);
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

    await unmount(component);
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

  function rateButton(label: string): HTMLButtonElement | null {
    return document.querySelector(`[aria-label="${label}"]`);
  }

  async function tooltipFor(element: Element): Promise<string | undefined> {
    element.dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
    await tick();
    const text = document.querySelector('[role="tooltip"]')?.textContent?.trim();
    element.dispatchEvent(new FocusEvent("focusout", { bubbles: true }));
    await tick();
    return text;
  }

  it("names each rate's value and explains its denominator on focus", async () => {
    seedRates();
    const component = mount(ToolUsage, { target: document.body });
    await tick();
    expect(document.body.textContent).toContain("11/12");
    expect(document.body.textContent).toContain("Rates leave out calls");
    const empty = rateButton("Grep Empty rate: 70.0%");
    expect(empty?.textContent?.trim()).toBe("70.0%");
    expect(rateButton("Grep Repeat rate: 45.5%")).not.toBeNull();
    const recovery = rateButton("Grep Recovery rate: 66.7%");
    expect(await tooltipFor(empty!)).toBe("7/10 empty results among known outcomes.");
    expect(await tooltipFor(recovery!)).toBe("2/3 recovered sequences. Open: 1. Unknown: 1.");
    await unmount(component);
  });

  it("keeps an exact zero inert and a tiny nonzero rate clickable", async () => {
    seedRates();
    analytics.tools!.by_tool[0]!.repeat_rate = 0;
    analytics.tools!.by_tool[0]!.empty_rate = 0.0003;
    const component = mount(ToolUsage, { target: document.body });
    await tick();
    expect(document.querySelector('[aria-label^="Grep Repeat rate"]')).toBeNull();
    const zero = [...document.querySelectorAll(".tool-rate")][1]!;
    expect(zero.textContent?.trim()).toBe("0.0%");
    expect(rateButton("Grep Empty rate: 0.03%")).not.toBeNull();
    await unmount(component);
  });

  it("formats rates and counts for the active locale", async () => {
    vi.mocked(getLocale).mockImplementation(() => "fr");
    seedRates();
    analytics.tools!.by_tool[0]!.call_count = 1200;
    analytics.tools!.by_tool[0]!.analyzed_calls = 1100;
    const component = mount(ToolUsage, { target: document.body });
    await tick();
    expect(document.querySelector(".tool-rate")?.textContent).toMatch(/70,0\s%/u);
    expect(document.body.textContent).toMatch(/1\s100\/1\s200/u);
    await unmount(component);
  });

  const example = {
    session_id: "recovered",
    project: "project-a",
    agent: "claude",
    date: "2025-06-02",
    outcome: "",
    health_score: null,
    health_grade: null,
    signal_total: 1,
    reason_code: "tool_recovery_rate",
    excerpt: "",
    message_ordinal: 2,
  };

  it("opens contributing sessions with the active filters and ignores an older response", async () => {
    seedRates();
    analytics.project = "project-a";
    analytics.model = "model-b";
    let resolveOlder!: (value: DbSignalSessionsResponse) => void;
    const older = new Promise<DbSignalSessionsResponse>((resolve) => (resolveOlder = resolve));
    const fetch = vi
      .spyOn(AnalyticsService, "getApiV1AnalyticsSignalSessions")
      .mockReturnValueOnce(older)
      .mockResolvedValue({ signal: "tool_recovery_rate", total: 1, sessions: [example] });
    const component = mount(ToolUsage, { target: document.body });
    await tick();
    rateButton("Grep Empty rate: 70.0%")!.click();
    await tick();
    rateButton("Grep Recovery rate: 66.7%")!.click();
    await tick();
    await tick();
    expect(fetch).toHaveBeenLastCalledWith(
      expect.objectContaining({
        project: "project-a",
        model: "model-b",
        tool_name: "Grep",
        tool_category: "Grep",
        signal: "tool_recovery_rate",
        offset: 0,
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
    await unmount(component);
  });

  it("opens the session at the message holding the first matching call", async () => {
    seedRates();
    vi.spyOn(AnalyticsService, "getApiV1AnalyticsSignalSessions").mockResolvedValue({
      signal: "tool_empty_rate",
      total: 1,
      sessions: [example],
    });
    const navigate = vi.spyOn(router, "navigateToSession").mockImplementation(() => {});
    const component = mount(ToolUsage, { target: document.body });
    await tick();
    rateButton("Grep Empty rate: 70.0%")!.click();
    await tick();
    await tick();
    const row = document.querySelector<HTMLAnchorElement>("a.evidence-row")!;
    expect(row.getAttribute("href")).toContain("msg=2");
    row.click();
    expect(navigate).toHaveBeenLastCalledWith("recovered", { msg: "2" });
    await unmount(component);
  });

  it("explains an unavailable rate on focus", async () => {
    seedRates();
    const tool = analytics.tools!.by_tool[0]!;
    Object.assign(tool, {
      recovery_rate: null,
      recovered_sequences: 0,
      abandoned_sequences: 0,
      open_sequences: 2,
      unknown_sequences: 1,
    });
    const component = mount(ToolUsage, { target: document.body });
    await tick();
    const cell = [...document.querySelectorAll(".tool-rate")][2]!;
    expect(cell.textContent?.trim()).toBe("Unavailable");
    expect(await tooltipFor(cell.querySelector(".kit-tooltip-trigger")!)).toBe(
      "0/0 recovered sequences. Open: 2. Unknown: 1.",
    );
    await unmount(component);
  });

  it("keeps same-named tools in different categories apart and pages through sessions", async () => {
    seedRates();
    const row = analytics.tools!.by_tool[0]!;
    analytics.tools!.by_tool = [
      { ...row, tool_name: "readFile", category: "Read" },
      { ...row, tool_name: "readFile", category: "MCP" },
    ];
    const fetch = vi.spyOn(AnalyticsService, "getApiV1AnalyticsSignalSessions").mockResolvedValue({
      signal: "tool_empty_rate",
      total: 2,
      next_offset: 1,
      sessions: [example],
    });
    const component = mount(ToolUsage, { target: document.body });
    await tick();
    const buttons = document.querySelectorAll('[aria-label="readFile Empty rate: 70.0%"]');
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
    fetch.mockResolvedValue({
      signal: "tool_empty_rate",
      total: 2,
      sessions: [{ ...example, session_id: "second" }],
    });
    more.click();
    await tick();
    await tick();
    expect(fetch).toHaveBeenLastCalledWith(
      expect.objectContaining({ tool_category: "Read", offset: 1 }),
      expect.anything(),
    );
    expect(document.querySelectorAll("a.evidence-row")).toHaveLength(2);
    fetch.mockResolvedValue({ signal: "tool_empty_rate", total: 0, sessions: [] });
    (buttons[1] as HTMLButtonElement).click();
    await tick();
    await tick();
    expect(fetch).toHaveBeenLastCalledWith(
      expect.objectContaining({ tool_category: "MCP", offset: 0 }),
      expect.anything(),
    );
    expect(document.querySelector(".tool-evidence")?.textContent).toContain("0 sessions");
    await unmount(component);
  });

  it("keeps loaded pages across an unchanged refresh and reloads when a filter changes", async () => {
    seedRates();
    const fetch = vi
      .spyOn(AnalyticsService, "getApiV1AnalyticsSignalSessions")
      .mockResolvedValueOnce({
        signal: "tool_empty_rate",
        total: 2,
        next_offset: 1,
        sessions: [example],
      })
      .mockResolvedValueOnce({
        signal: "tool_empty_rate",
        total: 2,
        sessions: [{ ...example, session_id: "second" }],
      })
      .mockResolvedValue({ signal: "tool_empty_rate", total: 1, sessions: [example] });
    const component = mount(ToolUsage, { target: document.body });
    await tick();
    rateButton("Grep Empty rate: 70.0%")!.click();
    await tick();
    await tick();
    [...document.querySelectorAll("button")]
      .find((button) => button.textContent?.includes("More sessions"))!
      .click();
    await tick();
    await tick();
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(document.querySelectorAll("a.evidence-row")).toHaveLength(2);
    // A live refresh replaces the tool data with equal counts.
    analytics.tools = JSON.parse(JSON.stringify(analytics.tools));
    await tick();
    await tick();
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(document.querySelectorAll("a.evidence-row")).toHaveLength(2);
    analytics.project = "project-b";
    await tick();
    await tick();
    expect(fetch).toHaveBeenCalledTimes(3);
    expect(fetch).toHaveBeenLastCalledWith(
      expect.objectContaining({ project: "project-b", offset: 0 }),
      expect.anything(),
    );
    expect(document.querySelectorAll("a.evidence-row")).toHaveLength(1);
    // The selected tool leaves the results, so its evidence closes.
    analytics.tools = { ...analytics.tools!, by_tool: [] };
    await tick();
    expect(document.querySelector(".tool-evidence")).toBeNull();
    await unmount(component);
  });
});
