import type { CodexcloudHomeStatus } from "../api/generated/index.js";
import { getLocale, m } from "../i18n/index.js";
import { formatTimestamp } from "./format.js";

// States that need the user's attention; the status bar shows the first
// such row and colors it as a warning.
const SEVERITY: Record<string, number> = {
  error: 3,
  config_error: 3,
  retrying: 2,
};

function stateSummary(row: CodexcloudHomeStatus): string {
  switch (row.state) {
    case "waiting":
      return m.status_bar_codex_cloud_waiting();
    case "importing":
      if (row.total) {
        return m.status_bar_codex_cloud_importing_progress({
          imported: (row.imported ?? 0).toLocaleString(getLocale()),
          total: row.total.toLocaleString(getLocale()),
        });
      }
      return m.status_bar_codex_cloud_importing_found({
        count: row.found ?? 0,
        countLabel: (row.found ?? 0).toLocaleString(getLocale()),
      });
    case "up_to_date":
      return m.status_bar_codex_cloud_up_to_date({
        time: formatTimestamp(row.last_success),
      });
    case "retrying":
      return m.status_bar_codex_cloud_retrying({
        count: row.retry_count ?? 0,
        countLabel: (row.retry_count ?? 0).toLocaleString(getLocale()),
      });
    case "error":
      return m.status_bar_codex_cloud_error();
    case "config_error":
      return m.status_bar_codex_cloud_config_error();
    case "off_disabled_agents":
    case "off_disabled":
    case "off_no_sync":
      return m.status_bar_codex_cloud_off();
    default:
      // A state this build does not know; the server's message names it.
      return row.message || row.state;
  }
}

function rowDetail(row: CodexcloudHomeStatus): string[] {
  const lines = [
    m.status_bar_codex_cloud_homes({
      homes: row.homes.length > 0 ? row.homes.join(", ") : row.key,
    }),
    stateSummary(row),
  ];
  if (row.state === "retrying") {
    lines.push(
      m.status_bar_codex_cloud_oldest_failure({
        time: formatTimestamp(row.oldest_failure),
      }),
    );
    if (row.message) lines.push(row.message);
    lines.push(m.status_bar_codex_cloud_retry_note());
  } else if (row.message) {
    lines.push(row.message);
  }
  if (row.state === "error" && row.last_success) {
    lines.push(
      m.status_bar_codex_cloud_last_success({
        time: formatTimestamp(row.last_success),
      }),
    );
  }
  if (row.live) lines.push(row.live);
  if (row.plan_usage) lines.push(row.plan_usage);
  return lines;
}

export interface CodexCloudStatusLine {
  /** One short line for the status bar. */
  summary: string;
  /** Every row's state, live token, and plan usage lines, for a tooltip. */
  detail: string;
  /** Whether the summarized row needs the user's attention. */
  warn: boolean;
}

/**
 * Summarizes codex-cloud status rows for the status bar, or returns null
 * when there are none: the source is not configured.
 */
export function codexCloudStatusLine(rows: CodexcloudHomeStatus[]): CodexCloudStatusLine | null {
  if (rows.length === 0) return null;
  let shown = rows[0]!;
  for (const row of rows) {
    if ((SEVERITY[row.state] ?? 0) > (SEVERITY[shown.state] ?? 0)) shown = row;
  }
  return {
    summary: stateSummary(shown),
    detail: rows.map((row) => rowDetail(row).join("\n")).join("\n\n"),
    warn: (SEVERITY[shown.state] ?? 0) > 0,
  };
}
