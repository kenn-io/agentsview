import { formatTokenCount } from "@kenn-io/kit-ui/utils/format";
import { formatDateTime, m } from "../i18n/index.js";
export { formatMoney as formatCost } from "../money.js";

// These four helpers are byte-identical to kit-ui's implementations; the
// locale-aware / app-specific formatters below stay local. Note kit-ui keeps
// `truncate` in its time module, not format.
export { formatNumber, formatTokenCount } from "@kenn-io/kit-ui/utils/format";
export { truncate } from "@kenn-io/kit-ui/utils/time";

const MINUTE = 60;
const HOUR = 3600;
const DAY = 86400;
const WEEK = 604800;

/** Formats an ISO timestamp as a human-friendly relative time */
export function formatRelativeTime(isoString: string | null | undefined): string {
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
export function formatTimestamp(isoString: string | null | undefined): string {
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
export function formatAgentName(agent: string | null | undefined): string {
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

  const contextLabel = hasContextTokens ? `${formatTokenCount(contextTokens)} ctx` : "— ctx";
  const outputLabel = hasOutputTokens ? `${formatTokenCount(outputTokens)} out` : "— out";

  return `${contextLabel} / ${outputLabel}`;
}

let nonceCounter = 0;

/** Reset the nonce counter. Exported for testing only. */
export function _resetNonceCounter(value = 0): void {
  nonceCounter = value;
}

export interface HighlightSegment {
  text: string;
  match: boolean;
}

/**
 * Split plain snippet text into segments, flagging case-insensitive
 * occurrences of the query's words (two or more characters). Used to
 * emphasize matches when a backend returns snippets without <mark> tags.
 */
/**
 * Split a search query into terms the way the backend does (see
 * PrepareFTSQuery and FTSTerms in internal/db/search.go): a query that starts
 * with a double quote uses FTS syntax, where quoted runs are single terms and
 * a doubled quote inside one is a literal quote; any other query is literal
 * whitespace-separated words, quotes included.
 */
function searchTerms(raw: string): string[] {
  const query = raw.trim();
  if (!query.startsWith('"')) return query.split(/\s+/).filter(Boolean);
  const terms: string[] = [];
  let current = "";
  let inQuote = false;
  let hasTerm = false;
  for (let i = 0; i < query.length; i++) {
    const c = query[i]!;
    if (c === '"') {
      if (inQuote && query[i + 1] === '"') {
        current += '"';
        hasTerm = true;
        i++;
        continue;
      }
      inQuote = !inQuote;
      hasTerm = true;
    } else if (!inQuote && (c === " " || c === "\t" || c === "\n" || c === "\r")) {
      // FTSTerms splits only on ASCII space, tab, newline, and return here.
      if (hasTerm) terms.push(current);
      current = "";
      hasTerm = false;
    } else {
      current += c;
      hasTerm = true;
    }
  }
  if (hasTerm) terms.push(current);
  return terms;
}

export function highlightTerms(text: string, query: string): HighlightSegment[] {
  // Terms keep their exact spelling and the regex uses Unicode case folding,
  // so "İstanbul" and the Kelvin sign match as typed.
  const terms = [...new Set(searchTerms(query).filter((term) => term.length >= 2))]
    .sort((a, b) => b.length - a.length);
  if (terms.length === 0 || text === "") return [{ text, match: false }];
  const pattern = new RegExp(
    `(${terms.map((term) => term.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")).join("|")})`,
    "giu",
  );
  const segments: HighlightSegment[] = [];
  let last = 0;
  for (const found of text.matchAll(pattern)) {
    const start = found.index ?? 0;
    if (start > last) segments.push({ text: text.slice(last, start), match: false });
    segments.push({ text: found[0], match: true });
    last = start + found[0].length;
  }
  if (last < text.length) segments.push({ text: text.slice(last), match: false });
  return segments;
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
