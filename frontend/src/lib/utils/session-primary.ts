interface PrimaryCandidate {
  id: string;
  parent_session_id?: string | null;
  parent_session_ids?: string[];
  relationship_type?: string | null;
  started_at?: string | null;
  ended_at?: string | null;
  created_at?: string;
}

function recencyKey(s: PrimaryCandidate): string {
  return s.ended_at ?? s.started_at ?? s.created_at ?? "";
}

function parentIds(s: PrimaryCandidate): string[] {
  return s.parent_session_id ? [s.parent_session_id] : (s.parent_session_ids ?? []);
}

function indexById<T extends PrimaryCandidate>(
  sessions: readonly T[],
  ancestryPool: readonly T[],
): Map<string, T> {
  const byId = new Map<string, T>();
  for (const s of ancestryPool) byId.set(s.id, s);
  for (const s of sessions) if (!byId.has(s.id)) byId.set(s.id, s);
  return byId;
}

/**
 * Pick the session that heads a related-session group in the sidebar:
 * the group's root, which the session tree also shows at the top. Its
 * continuations, forks, and subagents list below it. When a filter removed
 * the root, the remaining session closest to it heads the group, and on a
 * tie the earlier entry in `sessions` wins.
 *
 * `ancestryPool` is the unfiltered group, so filtered-out ancestors still
 * count toward a session's distance from the root.
 */
export function selectPrimaryId<T extends PrimaryCandidate>(
  sessions: readonly T[],
  groupKey: string,
  ancestryPool: readonly T[] = sessions,
): string {
  if (sessions.length === 0) return groupKey;
  if (sessions.some((s) => s.id === groupKey)) return groupKey;

  const byId = indexById(sessions, ancestryPool);
  const depths = new Map<string, number>();
  const depthOf = (id: string, visiting: Set<string>): number => {
    if (id === groupKey) return 0;
    const known = depths.get(id);
    if (known !== undefined) return known;
    const s = byId.get(id);
    if (!s || visiting.has(id)) return Infinity;
    visiting.add(id);
    let depth = Infinity;
    for (const parent of parentIds(s)) {
      depth = Math.min(depth, depthOf(parent, visiting) + 1);
    }
    depths.set(id, depth);
    return depth;
  };

  let best = sessions[0]!;
  let bestDepth = depthOf(best.id, new Set());
  for (let i = 1; i < sessions.length; i++) {
    const s = sessions[i]!;
    const depth = depthOf(s.id, new Set());
    if (depth < bestDepth) {
      bestDepth = depth;
      best = s;
    }
  }
  return best.id;
}

/**
 * Pick the session whose state a related-session group shows: its status
 * dot and its place in the sidebar's status sort. On a recency tie the
 * earlier entry in `sessions` wins.
 *
 * Groups with subagents use their root. Otherwise the most recently active
 * session on the root's main line wins: a continuation carries the
 * conversation forward, so the newest one holds its current state, while a
 * fork branches off it and never stands in for the session it branched
 * from. When no main-line session is present, for example because a filter
 * removed them, every session is a candidate.
 *
 * `ancestryPool` is the unfiltered group, so filtered-out ancestors do not
 * hide a fork further up the chain.
 */
export function selectLiveId<T extends PrimaryCandidate>(
  sessions: readonly T[],
  groupKey: string,
  ancestryPool: readonly T[] = sessions,
): string {
  if (sessions.length === 0) return groupKey;
  if (sessions.some((s) => s.relationship_type === "subagent")) {
    const root = sessions.find((s) => s.id === groupKey);
    return root ? root.id : sessions[0]!.id;
  }

  const byId = indexById(sessions, ancestryPool);

  // A session is off the main line when it, or a loaded ancestor below
  // the group root, is a fork. The group root itself may be a fork whose
  // parent is not loaded; it still heads its own group.
  const branched = new Map<string, boolean>();
  const isBranched = (id: string, visiting: Set<string>): boolean => {
    const known = branched.get(id);
    if (known !== undefined) return known;
    if (id === groupKey || visiting.has(id)) return false;
    visiting.add(id);
    const s = byId.get(id);
    let result = false;
    if (s?.relationship_type === "fork") {
      result = true;
    } else if (s) {
      result = parentIds(s).some((parent) => isBranched(parent, visiting));
    }
    branched.set(id, result);
    return result;
  };

  const mainLine = sessions.filter((s) => !isBranched(s.id, new Set()));
  const candidates = mainLine.length > 0 ? mainLine : sessions;
  let best = candidates[0]!;
  let bestKey = recencyKey(best);
  for (let i = 1; i < candidates.length; i++) {
    const s = candidates[i]!;
    const k = recencyKey(s);
    if (k > bestKey) {
      bestKey = k;
      best = s;
    }
  }
  return best.id;
}
