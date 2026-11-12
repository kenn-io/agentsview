import type { DbRateLimitSnapshot, DbRateLimitWindow } from "../api/generated/index.js";
import { m } from "../i18n/index.js";
import { agentLabel } from "./agents.js";
import { formatRelativeTime, formatTimestamp } from "./format.js";
import { formatPlanNumber } from "./planUsage.js";

const MINUTES_PER_HOUR = 60;
const MINUTES_PER_DAY = 1440;

export interface RateLimitWindowRow {
  key: string;
  label: string;
  /** Share of the window used, clamped to 0-100 for the meter. */
  percent: number;
  usedText: string;
  /** When the window resets, or that it reset after the report. Empty when
   * the agent reported no reset time. */
  resetText: string;
  /** The window reset after this report, so the share is out of date. */
  resetSince: boolean;
}

export interface RateLimitGroup {
  key: string;
  title: string;
  reported: string;
  windows: RateLimitWindowRow[];
}

/** Names a window by the length the agent reported, or by its slot when the
 * agent reported no usable length. */
export function rateLimitWindowLabel(
  minutes: number | undefined,
  slot: "primary" | "secondary",
): string {
  if (minutes == null || minutes <= 0) {
    return slot === "primary" ? m.usage_limits_window_primary() : m.usage_limits_window_secondary();
  }
  if (minutes === 7 * MINUTES_PER_DAY) return m.usage_limits_window_weekly();
  if (minutes % MINUTES_PER_DAY === 0) {
    return m.usage_limits_window_days({ count: minutes / MINUTES_PER_DAY });
  }
  if (minutes % MINUTES_PER_HOUR === 0) {
    return m.usage_limits_window_hours({ count: minutes / MINUTES_PER_HOUR });
  }
  return m.usage_limits_window_minutes({ count: minutes });
}

function windowRow(
  snapshotKey: string,
  slot: "primary" | "secondary",
  window: DbRateLimitWindow,
  now: number,
): RateLimitWindowRow {
  const resetsAt = window.resets_at ? Date.parse(window.resets_at) : NaN;
  const resetSince = Number.isFinite(resetsAt) && resetsAt <= now;
  let resetText = "";
  if (resetSince) {
    resetText = m.usage_limits_reset_since();
  } else if (Number.isFinite(resetsAt)) {
    resetText = m.usage_limits_resets({ time: formatTimestamp(window.resets_at) });
  }
  return {
    key: `${snapshotKey}:${slot}`,
    label: rateLimitWindowLabel(window.window_minutes, slot),
    percent: Math.min(100, Math.max(0, window.used_percent)),
    usedText: m.usage_limits_used({ percent: formatPlanNumber(window.used_percent) }),
    resetText,
    resetSince,
  };
}

/**
 * Turns the latest rate-limit reports into display groups, one per agent,
 * account, and limit, in the order the API returns them (newest first).
 * Accounts are numbered only when one agent reports more than one.
 */
export function rateLimitGroups(
  snapshots: DbRateLimitSnapshot[],
  now: number = Date.now(),
): RateLimitGroup[] {
  const accounts = new Map<string, string[]>();
  for (const snap of snapshots) {
    const keys = accounts.get(snap.agent) ?? [];
    if (!keys.includes(snap.account_key)) keys.push(snap.account_key);
    accounts.set(snap.agent, keys);
  }
  return snapshots.map((snap) => {
    const key = `${snap.agent}:${snap.account_key}:${snap.limit_id}`;
    const parts = [agentLabel(snap.agent)];
    if (snap.limit_id && snap.limit_id !== snap.agent) parts.push(snap.limit_id);
    if (snap.plan_type) parts.push(snap.plan_type);
    const agentAccounts = accounts.get(snap.agent) ?? [];
    if (agentAccounts.length > 1) {
      parts.push(m.usage_limits_account({ number: agentAccounts.indexOf(snap.account_key) + 1 }));
    }
    const windows: RateLimitWindowRow[] = [];
    if (snap.primary) windows.push(windowRow(key, "primary", snap.primary, now));
    if (snap.secondary) windows.push(windowRow(key, "secondary", snap.secondary, now));
    return {
      key,
      title: parts.join(" · "),
      reported: m.usage_limits_reported({ time: formatRelativeTime(snap.observed_at) }),
      windows,
    };
  });
}
