import { describe, it, expect } from "vite-plus/test";
import {
  generateAnalyticsCSV,
  type AnalyticsData,
} from "./csv-export.js";

function emptyData(): AnalyticsData {
  return {
    from: "2025-01-01",
    to: "2025-01-31",
    summary: null,
    activity: null,
    projects: null,
    tools: null,
    velocity: null,
  };
}

describe("generateAnalyticsCSV", () => {
  it("returns empty string when all sections are null", () => {
    expect(generateAnalyticsCSV(emptyData())).toBe("");
  });

  it("generates summary section", () => {
    const data = emptyData();
    data.summary = {
      total_sessions: 10,
      total_messages: 200,
      total_output_tokens: 42000,
      token_reporting_sessions: 12,
      active_projects: 3,
      active_days: 15,
      avg_messages: 20,
      median_messages: 18,
      p90_messages: 35,
      most_active_project: "my-project",
      concentration: 0.456,
      agents: {},
    };

    const csv = generateAnalyticsCSV(data);
    const lines = csv.split("\n");

    expect(lines[0]).toBe("Summary");
    expect(lines[1]).toBe("Metric,Value");
    expect(lines[2]).toBe("Sessions,10");
    expect(lines[3]).toBe("Messages,200");
    expect(lines).toContainEqual("Output Tokens,42000");
    expect(lines).toContainEqual(
      "Token Reporting Sessions,12",
    );
    expect(lines).toContainEqual("Concentration,45.6%");
  });

  it("generates activity section with rows", () => {
    const data = emptyData();
    data.activity = {
      granularity: "day",
      series: [
        {
          date: "2025-01-01",
          sessions: 2,
          messages: 50,
          user_messages: 20,
          assistant_messages: 25,
          tool_calls: 5,
          thinking_messages: 0,
          by_agent: {},
        },
      ],
    };

    const csv = generateAnalyticsCSV(data);
    const lines = csv.split("\n");

    expect(lines[0]).toBe("Activity");
    expect(lines[2]).toBe("2025-01-01,2,50,20,25,5,0");
  });

  it("generates projects section", () => {
    const data = emptyData();
    data.projects = {
      projects: [
        {
          name: "proj-a",
          sessions: 5,
          messages: 100,
          first_session: "2025-01-01",
          last_session: "2025-01-10",
          avg_messages: 20,
          median_messages: 18,
          agents: {},
          daily_trend: 0.5,
        },
      ],
    };

    const csv = generateAnalyticsCSV(data);
    const lines = csv.split("\n");

    expect(lines[0]).toBe("Projects");
    expect(lines[2]).toBe("proj-a,5,100,20,18");
  });

  it("generates tools section with percentages", () => {
    const data = emptyData();
    data.tools = {
      total_calls: 100,
      by_category: [
        { category: "Read", count: 60, pct: 60 },
        { category: "Write", count: 40, pct: 40 },
      ],
      by_agent: [],
      trend: [],
    };

    const csv = generateAnalyticsCSV(data);
    expect(csv).toContain("Read,60,60%");
    expect(csv).toContain("Write,40,40%");
  });

  it("generates velocity section", () => {
    const data = emptyData();
    data.velocity = {
      overall: {
        turn_cycle_sec: { p50: 1.2, p90: 3.5 },
        first_response_sec: { p50: 0.5, p90: 1.8 },
        msgs_per_active_min: 4.2,
        chars_per_active_min: 150,
        tool_calls_per_active_min: 2.1,
      },
      by_agent: [],
      by_complexity: [],
    };

    const csv = generateAnalyticsCSV(data);
    expect(csv).toContain("Turn Cycle (sec),1.2,3.5");
    expect(csv).toContain("Msgs / Active Min,4.2,");
  });

  it("separates multiple sections with blank lines", () => {
    const data = emptyData();
    data.summary = {
      total_sessions: 1,
      total_messages: 1,
      total_output_tokens: 0,
      token_reporting_sessions: 0,
      active_projects: 1,
      active_days: 1,
      avg_messages: 1,
      median_messages: 1,
      p90_messages: 1,
      most_active_project: "p",
      concentration: 0.5,
      agents: {},
    };
    data.tools = {
      total_calls: 1,
      by_category: [
        { category: "Read", count: 1, pct: 100 },
      ],
      by_agent: [],
      trend: [],
    };

    const csv = generateAnalyticsCSV(data);
    expect(csv).toContain("\n\n");
    expect(csv.split("\n\n")).toHaveLength(2);
  });

  it("escapes CSV values with commas and quotes", () => {
    const data = emptyData();
    data.summary = {
      total_sessions: 1,
      total_messages: 1,
      total_output_tokens: 0,
      token_reporting_sessions: 0,
      active_projects: 1,
      active_days: 1,
      avg_messages: 1,
      median_messages: 1,
      p90_messages: 1,
      most_active_project: 'project, "special"',
      concentration: 0,
      agents: {},
    };

    const csv = generateAnalyticsCSV(data);
    expect(csv).toContain(
      '"project, ""special"""',
    );
  });

  it("escapes formula injection characters", () => {
    const data = emptyData();
    data.summary = {
      total_sessions: 1,
      total_messages: 1,
      total_output_tokens: 0,
      token_reporting_sessions: 0,
      active_projects: 1,
      active_days: 1,
      avg_messages: 1,
      median_messages: 1,
      p90_messages: 1,
      most_active_project: "=cmd()",
      concentration: 0,
      agents: {},
    };

    const csv = generateAnalyticsCSV(data);
    expect(csv).toContain("'=cmd()");
    expect(csv).not.toContain(",=cmd()");
  });
});
