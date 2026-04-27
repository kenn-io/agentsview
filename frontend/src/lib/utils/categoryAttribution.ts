export interface CallAttributionInput {
  category: string;
  durationMs: number | null;
  isSubagent: boolean;
  subagentRange?: { startedAtMs: number; endedAtMs: number };
}

export interface TurnAttributionInput {
  turnDurationMs: number | null;
  calls: CallAttributionInput[];
}

export interface TurnAttribution {
  category: string;
  durationMs: number;
}

export function attributeTurn(t: TurnAttributionInput): TurnAttribution | null {
  if (t.turnDurationMs == null) return null;
  if (t.calls.length === 0) return null;

  const subagents = t.calls.filter((c) => c.isSubagent && c.subagentRange);
  const nonSub = t.calls.filter((c) => !c.isSubagent);

  let remainder = t.turnDurationMs;
  let subUnion = 0;
  if (subagents.length > 0) {
    subUnion = unionDuration(
      subagents.map((c) => c.subagentRange!),
    );
    remainder = Math.max(0, t.turnDurationMs - subUnion);
  }

  // When a sub-agent's wall time meets or exceeds whatever non-
  // sub-agent work happened in the same turn, attribute the turn to
  // "Task". The user's mental model is "the sub-agent did the work"
  // — surfacing that on the turns lane is more honest than letting a
  // couple of fast parallel siblings win the count vote.
  if (subUnion > 0 && subUnion >= remainder) {
    return { category: "Task", durationMs: remainder };
  }

  if (nonSub.length === 0) {
    // All calls were sub-agents but none dominated; remainder is
    // pre/post overhead the parent turn carried. Attribute to Mixed.
    return { category: "Mixed", durationMs: remainder };
  }

  // Dominant non-sub-agent category: strict majority
  const counts = new Map<string, number>();
  for (const c of nonSub) {
    counts.set(c.category, (counts.get(c.category) ?? 0) + 1);
  }
  const total = nonSub.length;
  let dominant: string | null = null;
  for (const [cat, n] of counts) {
    if (n * 2 > total) {
      dominant = cat;
      break;
    }
  }

  return {
    category: dominant ?? "Mixed",
    durationMs: remainder,
  };
}

function unionDuration(
  ranges: Array<{ startedAtMs: number; endedAtMs: number }>,
): number {
  if (ranges.length === 0) return 0;
  const sorted = [...ranges].sort((a, b) => a.startedAtMs - b.startedAtMs);
  let total = 0;
  let curStart = sorted[0]!.startedAtMs;
  let curEnd = sorted[0]!.endedAtMs;
  for (let i = 1; i < sorted.length; i++) {
    const r = sorted[i]!;
    if (r.startedAtMs <= curEnd) {
      curEnd = Math.max(curEnd, r.endedAtMs);
    } else {
      total += curEnd - curStart;
      curStart = r.startedAtMs;
      curEnd = r.endedAtMs;
    }
  }
  total += curEnd - curStart;
  return Math.max(0, total);
}
