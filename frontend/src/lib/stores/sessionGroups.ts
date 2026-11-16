import { getSessionStatus, recencyKey } from "./sessionStatus.svelte.js";

export interface SessionGroupInput {
  id: string;
  parent_session_id?: string | null;
  parent_session_ids?: string[];
  relationship_type?: string | null;
  project: string;
  project_assigned?: boolean;
  machine: string;
  source_machines?: string[];
  agent: string;
  agent_label?: string | null;
  entrypoint?: string | null;
  first_message?: string | null;
  display_name?: string | null;
  started_at: string | null;
  ended_at: string | null;
  created_at: string;
  termination_status?: string | null;
  message_count: number;
  user_message_count?: number;
  transcript_revision?: string;
  is_automated?: boolean;
  is_teammate?: boolean;
  is_index_only?: boolean;
}

export interface SessionGroup {
  key: string;
  project: string;
  sessions: SessionGroupInput[];
  /** Unfiltered session list for ancestry classification.
   *  Set when a filter (e.g. starred) removes sessions from the group. */
  allSessions?: SessionGroupInput[];
  primarySessionId: string;
  totalMessages: number;
  startedAt: string | null;
  endedAt: string | null;
}

function maxString(a: string | null, b: string | null): string | null {
  if (a == null) return b;
  if (b == null) return a;
  return a > b ? a : b;
}

function minString(a: string | null, b: string | null): string | null {
  if (a == null) return b;
  if (b == null) return a;
  return a < b ? a : b;
}

/**
 * Walk parent_session_id chains to find the root session.
 * If a link is missing from the loaded set, the walk stops
 * there, forming a separate group for each disconnected
 * subchain.
 */
function findRoot(
  id: string,
  byId: Map<string, SessionGroupInput>,
  rootCache: Map<string, string>,
): string {
  const cached = rootCache.get(id);
  if (cached !== undefined) return cached;

  // Walk up, capping at set size to guard cycles.
  const visited = new Set<string>();
  let cur = id;
  while (true) {
    if (visited.has(cur)) break; // cycle guard
    visited.add(cur);
    const s = byId.get(cur);
    if (!s?.parent_session_id) break;
    const parent = s.parent_session_id;
    if (!byId.has(parent)) break; // missing link
    cur = parent;
  }

  // cur is the root — cache for every node we visited.
  for (const v of visited) {
    rootCache.set(v, cur);
  }
  return cur;
}

// A hosted equal-content child may belong to several proven parent cohorts.
// Keep every reachable loaded root, without choosing a representative parent.
function findPublicRoots(id: string, byId: Map<string, SessionGroupInput>): string[] {
  const pending = [id];
  const visited = new Set<string>();
  const roots = new Set<string>();
  while (pending.length) {
    const current = pending.pop()!;
    if (visited.has(current)) continue;
    visited.add(current);
    const row = byId.get(current);
    const parents = (
      row?.parent_session_ids?.length
        ? row.parent_session_ids
        : row?.parent_session_id
          ? [row.parent_session_id]
          : []
    ).filter((parent) => byId.has(parent));
    if (!parents.length) roots.add(current);
    else pending.push(...parents);
  }
  return [...roots].sort();
}

export function buildSessionGroups(sessions: SessionGroupInput[]): SessionGroup[] {
  const groups = groupSessions(sessions);
  return orderSessionGroups(groups, statusTiers(groups));
}

export function groupSessions(sessions: SessionGroupInput[]): SessionGroup[] {
  const byId = new Map<string, SessionGroupInput>();
  for (const s of sessions) {
    byId.set(s.id, s);
  }

  const rootCache = new Map<string, string>();
  const groupMap = new Map<string, SessionGroup>();
  const insertionOrder: string[] = [];

  const pluralParents = sessions.some((s) => s.parent_session_ids?.length);
  for (const s of sessions) {
    const roots = pluralParents ? findPublicRoots(s.id, byId) : [findRoot(s.id, byId, rootCache)];
    if (!roots.length) roots.push(findRoot(s.id, byId, rootCache));
    for (const root of roots) {
      // Sessions without a parent_session_id that aren't
      // pointed to by anyone get root == their own id, so
      // they form a single-session group naturally.
      const key = root;

      let group = groupMap.get(key);
      if (!group) {
        group = {
          key,
          project: s.project,
          sessions: [],
          primarySessionId: s.id,
          totalMessages: 0,
          startedAt: null,
          endedAt: null,
        };
        groupMap.set(key, group);
        insertionOrder.push(key);
      }

      group.sessions.push(s);
      group.totalMessages += s.message_count;
      group.startedAt = minString(group.startedAt, s.started_at);
      group.endedAt = maxString(group.endedAt, s.ended_at);
    }
  }

  // Adopt orphaned teammate sessions so they NEVER appear at root level.
  // A session with <teammate-message in first_message is always a child;
  // if parent_session_id is missing, adopt it into the nearest non-teammate
  // root group in the same project (no time limit).
  const isTeammateSession = (s: SessionGroupInput) =>
    s.is_teammate ?? s.first_message?.includes("<teammate-message") ?? false;

  const keysToRemove = new Set<string>();

  // Build a per-project index of non-teammate root groups for adoption.
  const adoptTargets = new Map<string, string[]>(); // project -> group keys
  for (const [key, group] of groupMap) {
    // A valid adoption target is any group whose root session is NOT a teammate.
    const root = group.sessions.find((s) => s.id === key) ?? group.sessions[0]!;
    if (!isTeammateSession(root)) {
      let list = adoptTargets.get(group.project);
      if (!list) {
        list = [];
        adoptTargets.set(group.project, list);
      }
      list.push(key);
    }
  }

  // Collect all orphaned teammate groups (including multi-session ones
  // where the root itself is a teammate, e.g. a teammate that spawned
  // subagents).
  const orphanGroups: Array<{ key: string; group: SessionGroup; time: number }> = [];
  for (const [key, group] of groupMap) {
    const root = group.sessions.find((s) => s.id === key) ?? group.sessions[0]!;
    if (!isTeammateSession(root)) continue;
    if (root.parent_session_id) continue; // linked but parent not loaded — leave as-is
    orphanGroups.push({
      key,
      group,
      time: new Date(root.started_at ?? root.created_at ?? "1970-01-01").getTime(),
    });
  }

  // Pass 1: adopt orphans into the nearest non-teammate group in same project.
  for (const orphan of orphanGroups) {
    const candidates = adoptTargets.get(orphan.group.project);
    if (!candidates || candidates.length === 0) continue;

    let bestKey: string | null = null;
    let bestDist = Infinity;
    for (const ck of candidates) {
      const cg = groupMap.get(ck)!;
      const primary = cg.sessions.find((ss) => ss.id === ck) ?? cg.sessions[0]!;
      const cTime = new Date(primary.started_at ?? primary.created_at ?? "1970-01-01").getTime();
      const dist = Math.abs(orphan.time - cTime);
      if (dist < bestDist) {
        bestDist = dist;
        bestKey = ck;
      }
    }

    if (bestKey) {
      const target = groupMap.get(bestKey)!;
      for (const s of orphan.group.sessions) {
        target.sessions.push(s);
        target.totalMessages += s.message_count;
        target.startedAt = minString(target.startedAt, s.started_at);
        target.endedAt = maxString(target.endedAt, s.ended_at);
      }
      keysToRemove.add(orphan.key);
    }
  }

  // Pass 2: any remaining orphan teammates (project has no non-teammate
  // root group) — cluster all from same project into one group.
  const stillOrphaned = new Map<string, string[]>(); // project -> orphan keys
  for (const orphan of orphanGroups) {
    if (keysToRemove.has(orphan.key)) continue;
    let list = stillOrphaned.get(orphan.group.project);
    if (!list) {
      list = [];
      stillOrphaned.set(orphan.group.project, list);
    }
    list.push(orphan.key);
  }
  for (const [, keys] of stillOrphaned) {
    if (keys.length < 2) continue;
    const targetKey = keys[0]!;
    const target = groupMap.get(targetKey)!;
    for (let i = 1; i < keys.length; i++) {
      const src = groupMap.get(keys[i]!)!;
      for (const s of src.sessions) {
        target.sessions.push(s);
        target.totalMessages += s.message_count;
        target.startedAt = minString(target.startedAt, s.started_at);
        target.endedAt = maxString(target.endedAt, s.ended_at);
      }
      keysToRemove.add(keys[i]!);
    }
  }

  // Remove adopted orphan groups from the map and insertion order.
  for (const key of keysToRemove) {
    groupMap.delete(key);
  }

  for (const group of groupMap.values()) {
    if (group.sessions.length > 1) {
      group.sessions.sort((a, b) => {
        const ta = a.started_at ?? "";
        const tb = b.started_at ?? "";
        return ta < tb ? -1 : ta > tb ? 1 : 0;
      });
    }

    // For groups containing subagent children, the root session
    // should always be the main entry (not the most recent child).
    const hasSubagents = group.sessions.some((s) => s.relationship_type === "subagent");
    if (hasSubagents) {
      const rootIdx = group.sessions.findIndex((s) => s.id === group.key);
      group.primarySessionId = rootIdx >= 0 ? group.sessions[rootIdx]!.id : group.sessions[0]!.id;
    } else {
      // For continuation chains, use the most recently active session.
      let bestIdx = 0;
      let bestKey = recencyKey(group.sessions[0]!);
      for (let i = 1; i < group.sessions.length; i++) {
        const k = recencyKey(group.sessions[i]!);
        if (k > bestKey) {
          bestKey = k;
          bestIdx = i;
        }
      }
      group.primarySessionId = group.sessions[bestIdx]!.id;
    }
  }

  return insertionOrder.filter((k) => !keysToRemove.has(k)).map((k) => groupMap.get(k)!);
}

export function statusTiers(groups: SessionGroup[]): string {
  return groups.map(statusSortKey).join("");
}

export function orderSessionGroups(groups: SessionGroup[], tiers: string): SessionGroup[] {
  const ordered = groups.map((group, i) => ({
    group,
    tier: Number(tiers[i]),
    freshness: groupFreshness(group),
  }));

  // Two-key sort:
  //   1. status priority — working → waiting → idle → stale →
  //      quiet → unclean. Awaiting-user rows sit above idle even
  //      when older, and unclean (terminated mid tool call) sinks
  //      to the very bottom so noise from old crashed sessions
  //      doesn't push live work off-screen.
  //   2. group freshness — within a tier, the group whose
  //      newest member was written most recently wins. Mirrors
  //      the time-since-last-update order the sidebar had before
  //      the status sort was added.
  ordered.sort((a, b) => {
    const sa = a.tier;
    const sb = b.tier;
    if (sa !== sb) return sa - sb;
    const ra = a.freshness;
    const rb = b.freshness;
    if (ra > rb) return -1;
    if (ra < rb) return 1;
    return 0;
  });
  return ordered.map(({ group }) => group);
}

function statusSortKey(group: SessionGroup): number {
  const primary = group.sessions.find((s) => s.id === group.primarySessionId) ?? group.sessions[0]!;
  const status = getSessionStatus(primary, group.sessions);
  switch (status) {
    case "working":
      return 0;
    case "waiting":
      return 1;
    case "idle":
      return 2;
    case "stale":
      return 3;
    case "quiet":
      return 4;
    case "unclean":
      return 5;
  }
  return 6;
}

function groupFreshness(group: SessionGroup): string {
  // The freshest activity across any member of the group. A
  // subagent child's recent write counts as the group's
  // freshness so a parent waiting on a running child is sorted
  // by the child's activity.
  let best = "";
  for (const s of group.sessions) {
    const k = recencyKey(s);
    if (k > best) best = k;
  }
  return best;
}
