import type { Session } from "../../api/types.js";
import type { SessionGroup } from "../../stores/sessions.svelte.js";

export const ITEM_HEIGHT = 42;
export const CHILD_ITEM_HEIGHT = 34;
export const TEAM_HEADER_HEIGHT = 28;
export const HEADER_HEIGHT = 28;
export const OVERSCAN = 10;
export const STORAGE_KEY = "agentsview-group-by-agent";
export const STORAGE_KEY_GROUP = "agentsview-group-mode";

export type GroupMode = "none" | "agent" | "project";

export interface GroupSection {
  label: string;
  groups: SessionGroup[];
}

/** @deprecated Use GroupSection */
export type AgentSection = GroupSection;

export interface DisplayItem {
  id: string;
  type: "header" | "session" | "team-group" | "subagent-group";
  label: string;
  count: number;
  group?: SessionGroup;
  /** For child items within an expanded continuation chain. */
  session?: Session;
  /** True when this is a child session inside an expanded group. */
  isChild?: boolean;
  /** Nesting depth: 0 = root, 1 = child/team-group, 2 = teammate. */
  depth?: number;
  /** True when this is the last sibling at its depth level. */
  isLastChild?: boolean;
  /** True when this depth-1 item has a following depth-1 sibling
   *  (used to draw the vertical connector line through this row). */
  hasNextSiblingAtDepth1?: boolean;
  height: number;
  top: number;
}

/**
 * Read persisted group mode from localStorage, migrating the
 * legacy boolean key if needed.
 */
export function getInitialGroupMode(): GroupMode {
  if (typeof localStorage === "undefined") return "none";
  const stored = localStorage.getItem(STORAGE_KEY_GROUP);
  if (stored === "agent" || stored === "project") return stored;
  // Legacy migration
  if (localStorage.getItem(STORAGE_KEY) === "true") return "agent";
  return "none";
}

/**
 * Select the best primary session from a list using the same
 * recency rule as buildSessionGroups: for groups with subagents
 * prefer the root session (matching the group key), otherwise
 * pick the most recently active session.
 */
export function selectPrimaryId(
  sessions: Session[],
  groupKey: string,
): string {
  if (sessions.length === 0) return groupKey;
  const hasSubagents = sessions.some(
    (s) => s.relationship_type === "subagent",
  );
  if (hasSubagents) {
    const root = sessions.find((s) => s.id === groupKey);
    return root ? root.id : sessions[0]!.id;
  }
  let best = sessions[0]!;
  let bestKey = best.ended_at ?? best.started_at ?? best.created_at;
  for (let i = 1; i < sessions.length; i++) {
    const s = sessions[i]!;
    const k = s.ended_at ?? s.started_at ?? s.created_at;
    if (k > bestKey) {
      bestKey = k;
      best = s;
    }
  }
  return best.id;
}

/**
 * Build grouped sections from flat session groups.
 * Groups by agent name or project depending on mode.
 * Returns empty array when mode is "none".
 */
export function buildGroupSections(
  groups: SessionGroup[],
  mode: GroupMode,
): GroupSection[] {
  if (mode === "none") return [];
  const map = new Map<string, SessionGroup[]>();
  for (const g of groups) {
    const primary =
      g.sessions.find((s) => s.id === g.primarySessionId) ??
      g.sessions[0];
    if (!primary) continue;
    const key = mode === "agent" ? primary.agent : primary.project;
    let list = map.get(key);
    if (!list) {
      list = [];
      map.set(key, list);
    }
    list.push(g);
  }
  // Sort by count descending (most sessions first).
  return Array.from(map.entries())
    .sort((a, b) => b[1].length - a[1].length)
    .map(([label, groups]) => ({ label, groups }));
}

/** @deprecated Use buildGroupSections */
export function buildAgentSections(
  groups: SessionGroup[],
  groupByAgent: boolean,
): GroupSection[] {
  return buildGroupSections(groups, groupByAgent ? "agent" : "none");
}

/** Check if a session is a teammate (received a <teammate-message>). */
function isTeammateByMessage(s: Session): boolean {
  return s.first_message?.includes("<teammate-message") ?? false;
}

/**
 * Check if a session is a teammate, inheriting status from the
 * parent.  Continuation sessions of a teammate don't carry the
 * `<teammate-message>` tag themselves, but they belong to the
 * same teammate chain.
 */
function isTeammate(s: Session, allSessions: Session[]): boolean {
  if (isTeammateByMessage(s)) return true;
  // Walk up the parent chain within the group to inherit.
  if (s.parent_session_id) {
    const visited = new Set<string>();
    let cur: Session | undefined = s;
    while (cur?.parent_session_id && !visited.has(cur.id)) {
      visited.add(cur.id);
      const parent = allSessions.find((p) => p.id === cur!.parent_session_id);
      if (!parent) break;
      if (isTeammateByMessage(parent)) return true;
      cur = parent;
    }
  }
  return false;
}

/**
 * Check if a session is a subagent (has relationship_type === "subagent").
 * Continuation/fork sessions are NOT subagents.
 */
function isSubagent(s: Session): boolean {
  return s.relationship_type === "subagent";
}

/**
 * Check if a session is a subagent descendant — either it has
 * relationship_type === "subagent" itself, or one of its ancestors
 * in the group does.  This ensures that continuations/forks of a
 * subagent stay under the "Subagents" group header instead of
 * being miscategorised as plain continuations.
 */
export function isSubagentDescendant(
  s: Session,
  groupSessions: Session[],
): boolean {
  if (isSubagent(s)) return true;
  if (!s.parent_session_id) return false;
  const visited = new Set<string>();
  let cur: Session | undefined = s;
  while (cur?.parent_session_id && !visited.has(cur.id)) {
    visited.add(cur.id);
    const parent = groupSessions.find(
      (p) => p.id === cur!.parent_session_id,
    );
    if (!parent) break;
    if (isSubagent(parent)) return true;
    cur = parent;
  }
  return false;
}

/**
 * Check if a session is a continuation or fork (not a subagent
 * descendant, not a teammate).  These render without a sub-group
 * header or under a "Continuations" label.
 */
function isContinuation(s: Session, allSessions: Session[]): boolean {
  return !isSubagentDescendant(s, allSessions) && !isTeammate(s, allSessions);
}

/**
 * Emit display items for a single SessionGroup, expanding
 * child sessions when the group key is in expandedGroups.
 *
 * When a group contains teammate sessions, they are placed
 * under a synthetic "Team (N)" expandable node at depth 1,
 * and the teammates themselves render at depth 2. Regular
 * subagents remain at depth 1. This gives the 3-level tree:
 *   Session (depth 0) > Subagent (depth 1)
 *   Session (depth 0) > Team (depth 1) > Teammate (depth 2)
 */
function emitGroupItems(
  g: SessionGroup,
  label: string,
  expandedGroups: Set<string>,
  items: DisplayItem[],
  y: { value: number },
): void {
  const hasChildren = g.sessions.length > 1;
  const isExpanded = hasChildren && expandedGroups.has(g.key);

  // Primary session (depth 0)
  items.push({
    id: label ? `session:${label}:${g.primarySessionId}` : `session:${g.primarySessionId}`,
    type: "session",
    label,
    count: 0,
    group: g,
    depth: 0,
    height: ITEM_HEIGHT,
    top: y.value,
  });
  y.value += ITEM_HEIGHT;

  if (!isExpanded) return;

  // Separate children into subagents, teammates, and continuations.
  // Use allSessions (unfiltered) for ancestry classification so that
  // filtered-out ancestors don't break the parent-chain walk.
  const children = g.sessions.filter((s) => s.id !== g.primarySessionId);
  const ancestryPool = g.allSessions ?? g.sessions;
  const subagents: Session[] = [];
  const teammates: Session[] = [];
  const continuations: Session[] = [];
  for (const s of children) {
    if (isSubagentDescendant(s, ancestryPool)) {
      subagents.push(s);
    } else if (isTeammate(s, ancestryPool)) {
      teammates.push(s);
    } else {
      continuations.push(s);
    }
  }

  // Sort each child group most-recent-first so the sidebar
  // matches the main session list ordering convention.
  const byRecencyDesc = (a: Session, b: Session) => {
    const ka = a.ended_at ?? a.started_at ?? a.created_at;
    const kb = b.ended_at ?? b.started_at ?? b.created_at;
    return ka > kb ? -1 : ka < kb ? 1 : 0;
  };
  continuations.sort(byRecencyDesc);
  subagents.sort(byRecencyDesc);
  teammates.sort(byRecencyDesc);

  // Continuations render inline at depth 1 (no sub-group header).
  for (let i = 0; i < continuations.length; i++) {
    const s = continuations[i]!;
    // Determine if this is the last depth-1 sibling (accounting
    // for subsequent sub-group headers).
    const hasFollowingGroup = subagents.length > 0 || teammates.length > 0;
    const isLast = i === continuations.length - 1 && !hasFollowingGroup;
    items.push({
      id: `child:${s.id}`,
      type: "session",
      label,
      count: 0,
      group: g,
      session: s,
      isChild: true,
      depth: 1,
      isLastChild: isLast,
      height: CHILD_ITEM_HEIGHT,
      top: y.value,
    });
    y.value += CHILD_ITEM_HEIGHT;
  }

  // Count depth-1 group headers (subagents + team).
  const hasSubagentGroup = subagents.length > 0;
  const hasTeamGroup = teammates.length > 0;
  const depth1Count = (hasSubagentGroup ? 1 : 0) + (hasTeamGroup ? 1 : 0);
  let depth1Idx = 0;

  // Emit "Subagents (N)" group header + children at depth 2.
  if (hasSubagentGroup) {
    const subKey = `subagent:${g.key}`;
    const subExpanded = expandedGroups.has(subKey);

    items.push({
      id: `subagent-group:${g.key}`,
      type: "subagent-group",
      label: "Subagents",
      count: subagents.length,
      group: g,
      depth: 1,
      isLastChild: depth1Idx === depth1Count - 1,
      height: TEAM_HEADER_HEIGHT,
      top: y.value,
    });
    y.value += TEAM_HEADER_HEIGHT;
    depth1Idx++;

    if (subExpanded) {
      for (let i = 0; i < subagents.length; i++) {
        const s = subagents[i]!;
        items.push({
          id: `child:${s.id}`,
          type: "session",
          label,
          count: 0,
          group: g,
          session: s,
          isChild: true,
          depth: 2,
          isLastChild: i === subagents.length - 1,
          height: CHILD_ITEM_HEIGHT,
          top: y.value,
        });
        y.value += CHILD_ITEM_HEIGHT;
      }
    }
  }

  // Emit "Team (N)" group header + children at depth 2.
  if (hasTeamGroup) {
    const teamKey = `team:${g.key}`;
    const teamExpanded = expandedGroups.has(teamKey);

    items.push({
      id: `team-group:${g.key}`,
      type: "team-group",
      label: "Team",
      count: teammates.length,
      group: g,
      depth: 1,
      isLastChild: depth1Idx === depth1Count - 1,
      height: TEAM_HEADER_HEIGHT,
      top: y.value,
    });
    y.value += TEAM_HEADER_HEIGHT;

    if (teamExpanded) {
      for (let i = 0; i < teammates.length; i++) {
        const s = teammates[i]!;
        items.push({
          id: `child:${s.id}`,
          type: "session",
          label,
          count: 0,
          group: g,
          session: s,
          isChild: true,
          depth: 2,
          isLastChild: i === teammates.length - 1,
          height: CHILD_ITEM_HEIGHT,
          top: y.value,
        });
        y.value += CHILD_ITEM_HEIGHT;
      }
    }
  }
}

/**
 * Build a flat list of DisplayItems for virtual scrolling.
 * When mode is "none", produces a simple flat list.
 * Otherwise, interleaves header rows and session rows,
 * respecting collapsed groups. Continuation chains expand
 * inline when their group key is in expandedGroups.
 */
export function buildDisplayItems(
  groups: SessionGroup[],
  sections: GroupSection[],
  mode: GroupMode,
  collapsed: Set<string>,
  expandedGroups: Set<string>,
): DisplayItem[] {
  const y = { value: 0 };

  if (mode === "none") {
    const items: DisplayItem[] = [];
    for (const g of groups) {
      emitGroupItems(g, "", expandedGroups, items, y);
    }
    return items;
  }

  const items: DisplayItem[] = [];
  for (const section of sections) {
    items.push({
      id: `header:${section.label}`,
      type: "header",
      label: section.label,
      count: section.groups.length,
      height: HEADER_HEIGHT,
      top: y.value,
    });
    y.value += HEADER_HEIGHT;

    if (!collapsed.has(section.label)) {
      for (const g of section.groups) {
        emitGroupItems(g, section.label, expandedGroups, items, y);
      }
    }
  }
  return items;
}

/**
 * Compute total pixel height of the display items list.
 */
export function computeTotalSize(displayItems: DisplayItem[]): number {
  if (displayItems.length === 0) return 0;
  const last = displayItems[displayItems.length - 1]!;
  return last.top + last.height;
}

/**
 * Binary search for the index of the first visible item given
 * scrollY position.  Accounts for OVERSCAN rows before the
 * viewport.
 */
export function findStart(
  displayItems: DisplayItem[],
  scrollY: number,
): number {
  const target = scrollY - OVERSCAN * ITEM_HEIGHT;
  let lo = 0;
  let hi = displayItems.length - 1;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (displayItems[mid]!.top + displayItems[mid]!.height <= target) {
      lo = mid + 1;
    } else {
      hi = mid;
    }
  }
  return Math.max(0, lo);
}
