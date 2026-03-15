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

  if (diffSec < MINUTE) return "just now";
  if (diffSec < HOUR) return `${Math.floor(diffSec / MINUTE)}m ago`;
  if (diffSec < DAY) return `${Math.floor(diffSec / HOUR)}h ago`;
  if (diffSec < WEEK) return `${Math.floor(diffSec / DAY)}d ago`;

  return date.toLocaleDateString(undefined, {
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
  return d.toLocaleString(undefined, {
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
