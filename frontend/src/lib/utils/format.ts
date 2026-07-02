import { formatTokenCount } from "@kenn-io/kit-ui/utils/format";
import { formatDateTime, m } from "../i18n/index.js";

// These four helpers are byte-identical to kit-ui's implementations; the
// locale-aware / app-specific formatters below stay local. Note kit-ui keeps
// `truncate` in its time module, not format.
export {
  formatCost,
  formatNumber,
  formatTokenCount,
} from "@kenn-io/kit-ui/utils/format";
export { truncate } from "@kenn-io/kit-ui/utils/time";

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

/** Formats an agent name for display */
export function formatAgentName(
  agent: string | null | undefined,
): string {
  if (!agent) return "Unknown";
  // Capitalize first letter
  return agent.charAt(0).toUpperCase() + agent.slice(1);
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
