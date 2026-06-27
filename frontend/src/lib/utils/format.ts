import { formatDateTime, m } from "../i18n/index.js";

const MINUTE = 60;
const HOUR = 3600;
const DAY = 86400;
const WEEK = 604800;

/** Formats an ISO timestamp as a human-friendly relative time */
export function formatRelativeTime(
  isoString: string | null | undefined,
): string {
  if (!isoString) return "—";

  const date = new Date(isoString);
  const diffSec = Math.floor((Date.now() - date.getTime()) / 1000);

  if (diffSec < MINUTE) return m.shared_relative_just_now();
  if (diffSec < HOUR) {
    return m.shared_relative_minutes_ago({
      count: Math.floor(diffSec / MINUTE),
    });
  }
  if (diffSec < DAY) {
    return m.shared_relative_hours_ago({
      count: Math.floor(diffSec / HOUR),
    });
  }
  if (diffSec < WEEK) {
    return m.shared_relative_days_ago({
      count: Math.floor(diffSec / DAY),
    });
  }

  return formatDateTime(date, {
    month: "short",
    day: "numeric",
  });
}

/** Formats an ISO timestamp as a readable date/time string */
export function formatTimestamp(
  isoString: string | null | undefined,
): string {
  if (!isoString) return "—";
  const d = new Date(isoString);
  return formatDateTime(d, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

/** Truncates a string with ellipsis */
export function truncate(s: string, maxLen: number): string {
  if (s.length <= maxLen) return s;
  return s.slice(0, maxLen - 1) + "\u2026";
}

/** Formats an agent name for display */
export function formatAgentName(
  agent: string | null | undefined,
): string {
  if (!agent) return "Unknown";
  // Capitalize first letter
  return agent.charAt(0).toUpperCase() + agent.slice(1);
}

/** Formats a number with commas */
export function formatNumber(n: number): string {
  return n.toLocaleString();
}

/** Formats a token count as a compact string (e.g. 1.2k, 3.5M) */
export function formatTokenCount(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) {
    const k = Math.floor(n / 100) / 10;
    return k % 1 === 0 ? `${Math.floor(k)}k` : `${k}k`;
  }
  const m = Math.floor(n / 100_000) / 10;
  return m % 1 === 0 ? `${Math.floor(m)}M` : `${m}M`;
}

export function formatTokenUsage(
  contextTokens: number,
  hasContextTokens: boolean,
  outputTokens: number,
  hasOutputTokens: boolean,
): string | null {
  if (!hasContextTokens && !hasOutputTokens) return null;

  const contextLabel = hasContextTokens
    ? `${formatTokenCount(contextTokens)} ctx`
    : "— ctx";
  const outputLabel = hasOutputTokens
    ? `${formatTokenCount(outputTokens)} out`
    : "— out";

  return `${contextLabel} / ${outputLabel}`;
}

/** Formats a USD cost as a compact string (e.g. $0.42, $12.34, $123) */
export function formatCost(v: number): string {
  if (v > 0 && v < 0.01) return "<$0.01";
  if (v >= 100) return `$${v.toFixed(0)}`;
  return `$${v.toFixed(2)}`;
}

let nonceCounter = 0;

/** Reset the nonce counter. Exported for testing only. */
export function _resetNonceCounter(value = 0): void {
  nonceCounter = value;
}

/**
 * Sanitize an HTML snippet from FTS search results.
 * Only allows <mark> tags for highlighting; strips everything else.
 */
export function sanitizeSnippet(html: string): string {
  let nonce: string;
  do {
    nonce = `\x00${(nonceCounter++).toString(36)}\x00`;
  } while (html.includes(nonce));

  const OPEN = `${nonce}O${nonce}`;
  const CLOSE = `${nonce}C${nonce}`;

  return html
    .replace(/<mark>/gi, OPEN)
    .replace(/<\/mark>/gi, CLOSE)
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replaceAll(OPEN, "<mark>")
    .replaceAll(CLOSE, "</mark>");
}
