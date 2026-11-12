/** Minimal shape that StatusDot / getSessionStatus need from a
 * row. Both the full `Session` and the lighter `TopSession`
 * (analytics top list) match it structurally — the recency
 * fields all have safe fallbacks via `??`. */
export interface SessionStatusInput {
  termination_status?: string | null;
  ended_at?: string | null;
  started_at?: string | null;
  created_at?: string;
}

export function recencyKey(s: SessionStatusInput): string {
  return s.ended_at ?? s.started_at ?? s.created_at ?? "";
}

const FRESH_MS = 60 * 1000;
const RECENTLY_ACTIVE_MS = 10 * 60 * 1000;
const STALE_MS = 60 * 60 * 1000;

/** Ticking timestamp that updates every 30s so derived
 *  recency checks stay reactive without manual triggers. */
let now = $state(Date.now());
setInterval(() => {
  now = Date.now();
}, 30_000);

export type SessionStatus = "working" | "waiting" | "idle" | "stale" | "unclean" | "quiet";

/** Combine wall-clock recency with the parser's structural fact
 * (termination_status) into a single user-facing status.
 *
 * Precedence (first match wins, see body below):
 *   - waiting: < 10m idle AND termination_status == awaiting_user
 *   - working: < 1m idle AND not awaiting_user
 *   - idle:    1-10m idle AND not awaiting_user
 *   - quiet:   ≥ 10m idle AND clean/NULL
 *   - stale:   10-60m idle AND tool_call_pending/truncated
 *   - unclean: ≥ 60m idle AND tool_call_pending/truncated
 *
 * When a `groupSessions` array is provided, the freshness check
 * uses the freshest activity across the whole group. Two interactions
 * matter:
 *
 *   1. A parent in tool_call_pending whose subagent is currently
 *      writing rolls up to "working" via the freshest member — the
 *      tool_call_pending flag is not consulted at the working/idle
 *      branch, only at the stale/unclean branch.
 *   2. A parent in awaiting_user always renders "waiting" within the
 *      10m window even when a fork or sibling in the group is fresh.
 *      The parser flag is the stronger signal here: the agent has
 *      explicitly said "your turn".
 *
 * The parser flag always comes from the row's own session (the
 * parent's file is what's actually ambiguous), never from a child.
 *
 * Yellow (stale) and red (unclean) only fire when the parser has
 * positively flagged the session. Cleanly-finished or unclassified
 * sessions go straight from active → quiet — short-lived sessions
 * that complete normally don't pollute the sidebar with stale dots. */
export function getSessionStatus(
  session: SessionStatusInput,
  groupSessions?: SessionStatusInput[],
): SessionStatus {
  let freshest = recencyKey(session);
  if (groupSessions && groupSessions.length > 1) {
    for (const g of groupSessions) {
      const k = recencyKey(g);
      if (k > freshest) freshest = k;
    }
  }
  const ts = new Date(freshest).getTime();
  const age = now - ts;
  const term = session.termination_status;
  const flagged = term === "tool_call_pending" || term === "truncated";
  const awaitingUser = term === "awaiting_user";

  // awaiting_user wins over the freshness tier as soon as the
  // parser classifies it. The agent already told us "I'm done,
  // your turn", so we surface the waiting bubble even when a
  // related session in the group (e.g. a fork running in
  // parallel) is currently writing. For tool_call_pending parents
  // the freshness rollup still does its job — that flag isn't
  // checked here, so a parent in tool_call_pending with a fresh
  // subagent falls through to "working" below.
  if (awaitingUser && age < RECENTLY_ACTIVE_MS) return "waiting";

  if (age < FRESH_MS) return "working";
  if (age < RECENTLY_ACTIVE_MS) return "idle";
  if (!flagged) return "quiet";
  if (age < STALE_MS) return "stale";
  return "unclean";
}
