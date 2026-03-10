import type { SessionGroup } from "../../stores/sessions.svelte.js";

export const ITEM_HEIGHT = 42;
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
  type: "header" | "session";
  label: string;
  count: number;
  group?: SessionGroup;
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

/**
 * Build a flat list of DisplayItems for virtual scrolling.
 * When mode is "none", produces a simple flat list.
 * Otherwise, interleaves header rows and session rows,
 * respecting collapsed groups.
 */
export function buildDisplayItems(
  groups: SessionGroup[],
  sections: GroupSection[],
  mode: GroupMode,
  collapsed: Set<string>,
): DisplayItem[] {
  if (mode === "none") {
    return groups.map((g, i) => ({
      id: `session:${g.primarySessionId}`,
      type: "session" as const,
      label: "",
      count: 0,
      group: g,
      height: ITEM_HEIGHT,
      top: i * ITEM_HEIGHT,
    }));
  }

  const items: DisplayItem[] = [];
  let y = 0;
  for (const section of sections) {
    items.push({
      id: `header:${section.label}`,
      type: "header",
      label: section.label,
      count: section.groups.length,
      height: HEADER_HEIGHT,
      top: y,
    });
    y += HEADER_HEIGHT;

    if (!collapsed.has(section.label)) {
      for (const g of section.groups) {
        items.push({
          id: `session:${section.label}:${g.primarySessionId}`,
          type: "session",
          label: section.label,
          count: 0,
          group: g,
          height: ITEM_HEIGHT,
          top: y,
        });
        y += ITEM_HEIGHT;
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
