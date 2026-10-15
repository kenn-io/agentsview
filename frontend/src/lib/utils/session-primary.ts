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

/**
 * Pick the session that represents a related-session group in the
 * sidebar. On a recency tie the earlier entry in `sessions` wins.
 *
 * Groups with subagents show their root. Otherwise the most recently
 * active session wins among the sessions on the root's main line: a
 * continuation carries the conversation forward, while a fork branches
 * off it, so a fork and its descendants never replace the session they
 * branched from. When no main-line session is present, for example
 * because a filter removed them, every session is a candidate.
 *
 * `ancestryPool` is the unfiltered group, so filtered-out ancestors do
 * not hide a fork further up the chain.
 */
export function selectPrimaryId<T extends PrimaryCandidate>(
  sessions: readonly T[],
  groupKey: string,
  ancestryPool: readonly T[] = sessions,
): string {
  if (sessions.length === 0) return groupKey;
  if (sessions.some((s) => s.relationship_type === "subagent")) {
    const root = sessions.find((s) => s.id === groupKey);
    return root ? root.id : sessions[0]!.id;
  }

  const byId = new Map<string, T>();
  for (const s of ancestryPool) byId.set(s.id, s);
  for (const s of sessions) if (!byId.has(s.id)) byId.set(s.id, s);

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
      const parents = s.parent_session_id ? [s.parent_session_id] : (s.parent_session_ids ?? []);
      result = parents.some((parent) => isBranched(parent, visiting));
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
