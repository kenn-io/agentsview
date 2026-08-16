import type {
  AnalyticsSummary,
  ActivityResponse,
  ProjectsAnalyticsResponse,
  ToolsAnalyticsResponse,
  VelocityResponse,
} from "../api/types.js";

export interface AnalyticsData {
  from: string;
  to: string;
  summary: AnalyticsSummary | null;
  activity: ActivityResponse | null;
  projects: ProjectsAnalyticsResponse | null;
  tools: ToolsAnalyticsResponse | null;
  velocity: VelocityResponse | null;
}

function escapeCSV(value: string): string {
  // Prevent spreadsheet formula injection: prefix
  // dangerous leading characters with a single quote.
  if (/^[=+\-@\t\r\n]/.test(value)) {
    value = "'" + value;
  }
  if (
    value.includes(",") ||
    value.includes('"') ||
    value.includes("\n") ||
    value.includes("\r")
  ) {
    return `"${value.replace(/"/g, '""')}"`;
  }
  return value;
}

function row(cells: (string | number)[]): string {
  return cells.map((c) => escapeCSV(String(c))).join(",");
}

function buildTableSection<T>(
  title: string,
  headers: string[],
  items: T[],
  mapper: (item: T) => (string | number)[],
): string {
  return [
    title,
    row(headers),
    ...items.map((item) => row(mapper(item))),
  ].join("\n");
}

function buildSummarySection(
  summary: AnalyticsSummary,
): string {
  const outputTokens =
    summary.total_output_tokens === undefined
      ? ""
      : summary.total_output_tokens;
  const reportingSessions =
    summary.token_reporting_sessions === undefined
      ? ""
      : summary.token_reporting_sessions;
  const lines = [
    "Summary",
    row(["Metric", "Value"]),
    row(["Sessions", summary.total_sessions]),
    row(["Messages", summary.total_messages]),
    row(["Output Tokens", outputTokens]),
    row([
      "Token Reporting Sessions",
      reportingSessions,
    ]),
    row(["Active Projects", summary.active_projects]),
    row(["Active Days", summary.active_days]),
    row(["Avg Messages/Session", summary.avg_messages]),
    row([
      "Median Messages/Session",
      summary.median_messages,
    ]),
    row(["P90 Messages/Session", summary.p90_messages]),
    row([
      "Most Active Project",
      summary.most_active_project,
    ]),
    row([
      "Concentration",
      (summary.concentration * 100).toFixed(1) + "%",
    ]),
  ];
  return lines.join("\n");
}

function buildActivitySection(
  activity: ActivityResponse,
): string {
  return buildTableSection(
    "Activity",
    [
      "Date",
      "Sessions",
      "Messages",
      "User Messages",
      "Assistant Messages",
      "Tool Calls",
      "Thinking Messages",
    ],
    activity.series,
    (e) => [
      e.date,
      e.sessions,
      e.messages,
      e.user_messages,
      e.assistant_messages,
      e.tool_calls,
      e.thinking_messages,
    ],
  );
}

function buildProjectsSection(
  projects: ProjectsAnalyticsResponse,
): string {
  return buildTableSection(
    "Projects",
    [
      "Name",
      "Sessions",
      "Messages",
      "Avg Messages",
      "Median Messages",
    ],
    projects.projects,
    (p) => [
      p.name,
      p.sessions,
      p.messages,
      p.avg_messages,
      p.median_messages,
    ],
  );
}

function buildToolsSection(
  tools: ToolsAnalyticsResponse,
): string {
  return buildTableSection(
    "Tool Usage",
    ["Category", "Count", "Percentage"],
    tools.by_category,
    (c) => [c.category, c.count, `${c.pct}%`],
  );
}

function buildVelocitySection(
  velocity: VelocityResponse,
): string {
  const o = velocity.overall;
  const lines = [
    "Velocity",
    row(["Metric", "P50", "P90"]),
    row([
      "Turn Cycle (sec)",
      o.turn_cycle_sec.p50,
      o.turn_cycle_sec.p90,
    ]),
    row([
      "First Response (sec)",
      o.first_response_sec.p50,
      o.first_response_sec.p90,
    ]),
    row(["Msgs / Active Min", o.msgs_per_active_min, ""]),
    row([
      "Chars / Active Min",
      o.chars_per_active_min,
      "",
    ]),
    row([
      "Tools / Active Min",
      o.tool_calls_per_active_min,
      "",
    ]),
  ];
  return lines.join("\n");
}

export function generateAnalyticsCSV(
  data: AnalyticsData,
): string {
  return [
    data.summary && buildSummarySection(data.summary),
    data.activity && buildActivitySection(data.activity),
    data.projects && buildProjectsSection(data.projects),
    data.tools && buildToolsSection(data.tools),
    data.velocity && buildVelocitySection(data.velocity),
  ]
    .filter(Boolean)
    .join("\n\n");
}

function downloadCSV(
  csv: string,
  filename: string,
): void {
  if (!csv) return;
  const blob = new Blob([csv], { type: "text/csv" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 100);
}

export function exportAnalyticsCSV(
  data: AnalyticsData,
): void {
  const csv = generateAnalyticsCSV(data);
  downloadCSV(
    csv,
    `analytics-${data.from}-to-${data.to}.csv`,
  );
}
