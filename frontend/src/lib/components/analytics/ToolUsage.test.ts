// @vitest-environment jsdom
import {
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import ToolUsage from "./ToolUsage.svelte";
import { analytics } from "../../stores/analytics.svelte.js";

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
          tool_name: "Read",
          category: "Read",
          call_count: 3,
          session_count: 2,
          pct: 50,
        },
        {
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
});
