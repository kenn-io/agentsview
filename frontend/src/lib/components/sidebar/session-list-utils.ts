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
  type: "header" | "session" | "team-group";
  label: string;
  count: number;
  group?: SessionGroup;
  /** For child items within an expanded continuation chain. */
  session?: Session;
  /** True when this is a child session inside an expanded group. */
  isChild?: boolean;
  /** Nesting depth: 0 = root, 1 = child/team-group, 2 = teammate. */
  depth?: number;
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
function isTeammate(s: Session): boolean {
  return s.first_message?.includes("<teammate-message") ?? false;
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

  // Separate children into regular subagents and teammates.
  const children = g.sessions.filter((s) => s.id !== g.primarySessionId);
  const regulars: Session[] = [];
  const teammates: Session[] = [];
  for (const s of children) {
    if (isTeammate(s)) {
      teammates.push(s);
    } else {
      regulars.push(s);
    }
  }

  // Emit regular subagent children at depth 1.
  for (const s of regulars) {
    items.push({
      id: `child:${s.id}`,
      type: "session",
      label,
      count: 0,
      group: g,
      session: s,
      isChild: true,
      depth: 1,
      height: CHILD_ITEM_HEIGHT,
      top: y.value,
    });
    y.value += CHILD_ITEM_HEIGHT;
  }

  // Emit synthetic "Team" group node + teammate children at depth 2.
  if (teammates.length > 0) {
    const teamKey = `team:${g.key}`;
    const teamExpanded = expandedGroups.has(teamKey);

    items.push({
      id: `team-group:${g.key}`,
      type: "team-group",
      label: "Team",
      count: teammates.length,
      group: g,
      depth: 1,
      height: TEAM_HEADER_HEIGHT,
      top: y.value,
    });
    y.value += TEAM_HEADER_HEIGHT;

    if (teamExpanded) {
      for (const s of teammates) {
        items.push({
          id: `child:${s.id}`,
          type: "session",
          label,
          count: 0,
          group: g,
          session: s,
          isChild: true,
          depth: 2,
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
