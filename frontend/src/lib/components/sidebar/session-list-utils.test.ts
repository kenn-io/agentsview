import { describe, it, expect } from "vitest";
import type { Session } from "../../api/types.js";
import type { SessionGroup } from "../../stores/sessions.svelte.js";
import {
  ITEM_HEIGHT,
  CHILD_ITEM_HEIGHT,
  HEADER_HEIGHT,
  STORAGE_KEY,
  buildGroupSections,
  buildDisplayItems,
  computeTotalSize,
  findStart,
  isSubagentDescendant,
  selectPrimaryId,
} from "./session-list-utils.js";
import type { GroupSection, DisplayItem } from "./session-list-utils.js";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function makeSession(overrides: Partial<Session> = {}): Session {
  return {
    id: overrides.id ?? crypto.randomUUID(),
    project: "test-project",
    machine: "localhost",
    agent: "claude",
    first_message: "hello",
    started_at: "2025-01-01T00:00:00Z",
    ended_at: "2025-01-01T01:00:00Z",
    message_count: 10,
    user_message_count: 5,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    is_automated: false,
    created_at: "2025-01-01T00:00:00Z",
    ...overrides,
  };
}

function makeGroup(
  agent: string,
  sessionCount = 1,
  idPrefix?: string,
): SessionGroup {
  const prefix = idPrefix ?? agent;
  const sessions: Session[] = [];
  for (let i = 0; i < sessionCount; i++) {
    sessions.push(
      makeSession({
        id: `${prefix}-session-${i}`,
        agent,
      }),
    );
  }
  return {
    key: sessions[0]!.id,
    project: "test-project",
    sessions,
    primarySessionId: sessions[0]!.id,
    totalMessages: sessions.reduce((s, x) => s + x.message_count, 0),
    firstMessage: sessions[0]!.first_message,
    startedAt: sessions[0]!.started_at,
    endedAt: sessions[sessions.length - 1]!.ended_at,
  };
}

// ---------------------------------------------------------------------------
// buildGroupSections
// ---------------------------------------------------------------------------

describe("buildGroupSections", () => {
  it("returns empty array when mode is none", () => {
    const groups = [makeGroup("claude"), makeGroup("gpt")];
    const result = buildGroupSections(groups, "none");
    expect(result).toEqual([]);
  });

  it("groups session groups by agent name", () => {
    const groups = [
      makeGroup("claude", 1, "c1"),
      makeGroup("gpt", 1, "g1"),
      makeGroup("claude", 1, "c2"),
    ];
    const result = buildGroupSections(groups, "agent");

    expect(result).toHaveLength(2);
    const claudeSection = result.find((s) => s.label === "claude");
    const gptSection = result.find((s) => s.label === "gpt");
    expect(claudeSection).toBeDefined();
    expect(gptSection).toBeDefined();
    expect(claudeSection!.groups).toHaveLength(2);
    expect(gptSection!.groups).toHaveLength(1);
  });

  it("sorts sections by count descending", () => {
    const groups = [
      makeGroup("gpt", 1, "g1"),
      makeGroup("claude", 1, "c1"),
      makeGroup("claude", 1, "c2"),
      makeGroup("claude", 1, "c3"),
      makeGroup("gpt", 1, "g2"),
    ];
    const result = buildGroupSections(groups, "agent");

    expect(result[0]!.label).toBe("claude");
    expect(result[0]!.groups).toHaveLength(3);
    expect(result[1]!.label).toBe("gpt");
    expect(result[1]!.groups).toHaveLength(2);
  });

  it("uses primary session to determine agent", () => {
    const session = makeSession({ id: "primary-1", agent: "gemini" });
    const group: SessionGroup = {
      key: "primary-1",
      project: "test",
      sessions: [session],
      primarySessionId: "primary-1",
      totalMessages: 10,
      firstMessage: "hi",
      startedAt: "2025-01-01T00:00:00Z",
      endedAt: "2025-01-01T01:00:00Z",
    };
    const result = buildGroupSections([group], "agent");

    expect(result).toHaveLength(1);
    expect(result[0]!.label).toBe("gemini");
  });

  it("skips groups with no sessions", () => {
    const emptyGroup: SessionGroup = {
      key: "empty",
      project: "test",
      sessions: [],
      primarySessionId: "nonexistent",
      totalMessages: 0,
      firstMessage: null,
      startedAt: null,
      endedAt: null,
    };
    const result = buildGroupSections([emptyGroup], "agent");
    expect(result).toEqual([]);
  });

  it("groups by project when mode is project", () => {
    const groups = [
      makeGroup("claude", 1, "c1"),
      makeGroup("gpt", 1, "g1"),
    ];
    // Both groups have project "test-project" from makeGroup.
    const result = buildGroupSections(groups, "project");
    expect(result).toHaveLength(1);
    expect(result[0]!.label).toBe("test-project");
    expect(result[0]!.groups).toHaveLength(2);
  });
});

// ---------------------------------------------------------------------------
// buildDisplayItems — ungrouped mode
// ---------------------------------------------------------------------------

describe("buildDisplayItems (ungrouped)", () => {
  it("creates flat session items with correct ids", () => {
    const groups = [
      makeGroup("claude", 1, "a"),
      makeGroup("gpt", 1, "b"),
    ];
    const items = buildDisplayItems(groups, [], "none", new Set(), new Set());

    expect(items).toHaveLength(2);
    expect(items[0]!.type).toBe("session");
    expect(items[1]!.type).toBe("session");
    expect(items[0]!.id).toBe(`session:${groups[0]!.primarySessionId}`);
    expect(items[1]!.id).toBe(`session:${groups[1]!.primarySessionId}`);
  });

  it("assigns consecutive top positions using ITEM_HEIGHT", () => {
    const groups = [
      makeGroup("claude", 1, "a"),
      makeGroup("gpt", 1, "b"),
      makeGroup("gemini", 1, "c"),
    ];
    const items = buildDisplayItems(groups, [], "none", new Set(), new Set());

    for (let i = 0; i < items.length; i++) {
      expect(items[i]!.top).toBe(i * ITEM_HEIGHT);
      expect(items[i]!.height).toBe(ITEM_HEIGHT);
    }
  });

  it("attaches correct group reference", () => {
    const groups = [makeGroup("claude", 2, "a")];
    const items = buildDisplayItems(groups, [], "none", new Set(), new Set());

    expect(items).toHaveLength(1);
    expect(items[0]!.group).toBe(groups[0]);
  });

  it("returns empty array for no groups", () => {
    const items = buildDisplayItems([], [], "none", new Set(), new Set());
    expect(items).toEqual([]);
  });

  it("all ids are unique", () => {
    const groups = [
      makeGroup("claude", 1, "a"),
      makeGroup("claude", 1, "b"),
      makeGroup("gpt", 1, "c"),
    ];
    const items = buildDisplayItems(groups, [], "none", new Set(), new Set());
    const ids = items.map((i) => i.id);
    expect(new Set(ids).size).toBe(ids.length);
  });
});

// ---------------------------------------------------------------------------
// buildDisplayItems — grouped mode
// ---------------------------------------------------------------------------

describe("buildDisplayItems (grouped)", () => {
  function setup(opts?: { collapsed?: string[] }) {
    const groups = [
      makeGroup("claude", 1, "c1"),
      makeGroup("claude", 1, "c2"),
      makeGroup("gpt", 1, "g1"),
    ];
    const sections = buildGroupSections(groups, "agent");
    const collapsed = new Set(opts?.collapsed ?? []);
    const items = buildDisplayItems(groups, sections, "agent", collapsed, new Set());
    return { groups, sections, items };
  }

  it("interleaves headers and session items", () => {
    const { items } = setup();

    // claude section: 1 header + 2 sessions = 3
    // gpt section: 1 header + 1 session = 2
    expect(items).toHaveLength(5);
    expect(items[0]!.type).toBe("header");
    expect(items[0]!.label).toBe("claude");
    expect(items[1]!.type).toBe("session");
    expect(items[2]!.type).toBe("session");
    expect(items[3]!.type).toBe("header");
    expect(items[3]!.label).toBe("gpt");
    expect(items[4]!.type).toBe("session");
  });

  it("headers use HEADER_HEIGHT and sessions use ITEM_HEIGHT", () => {
    const { items } = setup();
    const headers = items.filter((i) => i.type === "header");
    const sessions = items.filter((i) => i.type === "session");

    for (const h of headers) {
      expect(h.height).toBe(HEADER_HEIGHT);
    }
    for (const s of sessions) {
      expect(s.height).toBe(ITEM_HEIGHT);
    }
  });

  it("top positions are calculated cumulatively", () => {
    const { items } = setup();

    let expectedTop = 0;
    for (const item of items) {
      expect(item.top).toBe(expectedTop);
      expectedTop += item.height;
    }
  });

  it("header items have correct count", () => {
    const { items } = setup();
    const claudeHeader = items.find(
      (i) => i.type === "header" && i.label === "claude",
    );
    const gptHeader = items.find(
      (i) => i.type === "header" && i.label === "gpt",
    );

    expect(claudeHeader!.count).toBe(2);
    expect(gptHeader!.count).toBe(1);
  });

  it("collapsed agents omit session items", () => {
    const { items } = setup({ collapsed: ["claude"] });

    // claude: header only (collapsed) = 1
    // gpt: header + 1 session = 2
    expect(items).toHaveLength(3);
    expect(items[0]!.type).toBe("header");
    expect(items[0]!.label).toBe("claude");
    expect(items[1]!.type).toBe("header");
    expect(items[1]!.label).toBe("gpt");
    expect(items[2]!.type).toBe("session");
  });

  it("collapsing all agents leaves only headers", () => {
    const { items } = setup({ collapsed: ["claude", "gpt"] });

    expect(items).toHaveLength(2);
    expect(items.every((i) => i.type === "header")).toBe(true);
  });

  it("collapsed agents still show correct header count", () => {
    const { items } = setup({ collapsed: ["claude"] });

    const claudeHeader = items.find(
      (i) => i.type === "header" && i.label === "claude",
    );
    expect(claudeHeader!.count).toBe(2);
  });

  it("all ids are unique across the entire array", () => {
    const { items } = setup();
    const ids = items.map((i) => i.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it("header ids differ from session ids with same agent", () => {
    const { items } = setup();
    const claudeHeader = items.find(
      (i) => i.type === "header" && i.label === "claude",
    );
    const claudeSessions = items.filter(
      (i) => i.type === "session" && i.label === "claude",
    );

    expect(claudeHeader!.id).toMatch(/^header:/);
    for (const s of claudeSessions) {
      expect(s.id).toMatch(/^session:/);
      expect(s.id).not.toBe(claudeHeader!.id);
    }
  });

  it("session ids in grouped mode include label prefix for uniqueness", () => {
    const { items } = setup();
    const sessionItems = items.filter((i) => i.type === "session");
    for (const s of sessionItems) {
      // Format: session:<label>:<primarySessionId>
      const parts = s.id.split(":");
      expect(parts.length).toBeGreaterThanOrEqual(3);
      expect(parts[0]).toBe("session");
      expect(parts[1]).toBe(s.label);
    }
  });
});

// ---------------------------------------------------------------------------
// computeTotalSize
// ---------------------------------------------------------------------------

describe("computeTotalSize", () => {
  it("returns 0 for empty array", () => {
    expect(computeTotalSize([])).toBe(0);
  });

  it("returns correct size for flat list", () => {
    const groups = [
      makeGroup("claude", 1, "a"),
      makeGroup("gpt", 1, "b"),
    ];
    const items = buildDisplayItems(groups, [], "none", new Set(), new Set());
    expect(computeTotalSize(items)).toBe(2 * ITEM_HEIGHT);
  });

  it("accounts for mixed header and session heights", () => {
    const groups = [
      makeGroup("claude", 1, "c1"),
      makeGroup("gpt", 1, "g1"),
    ];
    const sections = buildGroupSections(groups, "agent");
    const items = buildDisplayItems(groups, sections, "agent", new Set(), new Set());
    // claude: HEADER_HEIGHT + ITEM_HEIGHT
    // gpt:    HEADER_HEIGHT + ITEM_HEIGHT
    expect(computeTotalSize(items)).toBe(
      2 * HEADER_HEIGHT + 2 * ITEM_HEIGHT,
    );
  });

  it("smaller total when agents are collapsed", () => {
    const groups = [
      makeGroup("claude", 1, "c1"),
      makeGroup("claude", 1, "c2"),
      makeGroup("gpt", 1, "g1"),
    ];
    const sections = buildGroupSections(groups, "agent");
    const expanded = buildDisplayItems(
      groups,
      sections,
      "agent",
      new Set(),
      new Set(),
    );
    const collapsed = buildDisplayItems(
      groups,
      sections,
      "agent",
      new Set(["claude"]),
      new Set(),
    );

    expect(computeTotalSize(collapsed)).toBeLessThan(
      computeTotalSize(expanded),
    );
    // Difference should be exactly the two collapsed claude sessions.
    expect(
      computeTotalSize(expanded) - computeTotalSize(collapsed),
    ).toBe(2 * ITEM_HEIGHT);
  });
});

// ---------------------------------------------------------------------------
// findStart (binary search)
// ---------------------------------------------------------------------------

describe("findStart", () => {
  function flatItems(count: number): DisplayItem[] {
    const groups: SessionGroup[] = [];
    for (let i = 0; i < count; i++) {
      groups.push(makeGroup("claude", 1, `g${i}`));
    }
    return buildDisplayItems(groups, [], "none", new Set(), new Set());
  }

  it("returns 0 when scrolled to top", () => {
    const items = flatItems(100);
    expect(findStart(items, 0)).toBe(0);
  });

  it("returns 0 for negative scroll (clamped)", () => {
    const items = flatItems(100);
    expect(findStart(items, -100)).toBe(0);
  });

  it("returns index near the visible area", () => {
    const items = flatItems(100);
    // Scroll to row 50 (top = 50 * ITEM_HEIGHT)
    const scrollY = 50 * ITEM_HEIGHT;
    const start = findStart(items, scrollY);
    // Should be at most OVERSCAN rows before row 50.
    expect(start).toBeLessThanOrEqual(50);
    expect(start).toBeGreaterThanOrEqual(50 - 10); // OVERSCAN=10
  });

  it("returns last valid index when scrolled to end", () => {
    const items = flatItems(20);
    const scrollY = 20 * ITEM_HEIGHT;
    const start = findStart(items, scrollY);
    // Start should be within valid bounds.
    expect(start).toBeLessThan(items.length);
    expect(start).toBeGreaterThanOrEqual(0);
  });

  it("handles single-item list", () => {
    const items = flatItems(1);
    expect(findStart(items, 0)).toBe(0);
    expect(findStart(items, 1000)).toBe(0);
  });

  it("handles empty list", () => {
    // Edge case: empty items array.
    expect(findStart([], 0)).toBe(0);
  });

  it("works correctly with mixed-height items (grouped mode)", () => {
    const groups = [
      makeGroup("claude", 1, "c1"),
      makeGroup("claude", 1, "c2"),
      makeGroup("claude", 1, "c3"),
      makeGroup("gpt", 1, "g1"),
      makeGroup("gpt", 1, "g2"),
    ];
    const sections = buildGroupSections(groups, "agent");
    const items = buildDisplayItems(groups, sections, "agent", new Set(), new Set());

    // Scroll to where the gpt header would be visible.
    const start = findStart(items, 148);
    // Should return an index before 148 accounting for overscan.
    expect(start).toBeLessThanOrEqual(4);
    expect(start).toBeGreaterThanOrEqual(0);
  });
});

// ---------------------------------------------------------------------------
// STORAGE_KEY constant
// ---------------------------------------------------------------------------

describe("STORAGE_KEY", () => {
  it("has the expected value for localStorage persistence", () => {
    expect(STORAGE_KEY).toBe("agentsview-group-by-agent");
  });
});

// ---------------------------------------------------------------------------
// Unique id stability
// ---------------------------------------------------------------------------

describe("DisplayItem id stability", () => {
  it("produces identical ids for the same input", () => {
    const groups = [
      makeGroup("claude", 1, "c1"),
      makeGroup("gpt", 1, "g1"),
    ];
    const sections = buildGroupSections(groups, "agent");
    const items1 = buildDisplayItems(groups, sections, "agent", new Set(), new Set());
    const items2 = buildDisplayItems(groups, sections, "agent", new Set(), new Set());

    expect(items1.map((i) => i.id)).toEqual(items2.map((i) => i.id));
  });

  it("ungrouped ids are deterministic from primarySessionId", () => {
    const groups = [
      makeGroup("claude", 1, "x"),
      makeGroup("gpt", 1, "y"),
    ];
    const items = buildDisplayItems(groups, [], "none", new Set(), new Set());

    expect(items[0]!.id).toBe("session:x-session-0");
    expect(items[1]!.id).toBe("session:y-session-0");
  });

  it("grouped ids are deterministic from label + primarySessionId", () => {
    const groups = [makeGroup("claude", 1, "c1")];
    const sections = buildGroupSections(groups, "agent");
    const items = buildDisplayItems(groups, sections, "agent", new Set(), new Set());

    const sessionItem = items.find((i) => i.type === "session");
    expect(sessionItem!.id).toBe("session:claude:c1-session-0");
  });
});

// ---------------------------------------------------------------------------
// Starred-only count derivation
// ---------------------------------------------------------------------------

describe("starred-only session count", () => {
  function filterGroupsForStarred(
    groups: SessionGroup[],
    starredIds: Set<string>,
  ): SessionGroup[] {
    return groups
      .map((g) => {
        const filtered = g.sessions.filter((s) =>
          starredIds.has(s.id),
        );
        const primaryStillPresent = filtered.some(
          (s) => s.id === g.primarySessionId,
        );
        return {
          ...g,
          sessions: filtered,
          primarySessionId: primaryStillPresent
            ? g.primarySessionId
            : selectPrimaryId(filtered, g.key),
        };
      })
      .filter((g) => g.sessions.length > 0);
  }

  it("counts individual sessions, not groups", () => {
    const g1 = makeGroup("claude", 3, "c");
    const g2 = makeGroup("gpt", 3, "g");
    const groups = [g1, g2];

    const starred = new Set([
      "c-session-0",
      "c-session-2",
      "g-session-1",
    ]);
    const filtered = filterGroupsForStarred(groups, starred);

    const count = filtered.reduce(
      (n, g) => n + g.sessions.length,
      0,
    );
    expect(count).toBe(3);
    expect(filtered).toHaveLength(2);
  });

  it("excludes groups with no starred sessions", () => {
    const g1 = makeGroup("claude", 2, "c");
    const g2 = makeGroup("gpt", 2, "g");
    const groups = [g1, g2];

    const starred = new Set(["c-session-0"]);
    const filtered = filterGroupsForStarred(groups, starred);

    const count = filtered.reduce(
      (n, g) => n + g.sessions.length,
      0,
    );
    expect(count).toBe(1);
    expect(filtered).toHaveLength(1);
  });

  it("returns zero when nothing is starred", () => {
    const groups = [makeGroup("claude", 3, "c")];
    const filtered = filterGroupsForStarred(
      groups,
      new Set(),
    );

    const count = filtered.reduce(
      (n, g) => n + g.sessions.length,
      0,
    );
    expect(count).toBe(0);
    expect(filtered).toHaveLength(0);
  });

  it("preserves ancestry classification via allSessions when parent is unstarred", () => {
    // root -> teammate (unstarred) -> continuation (starred)
    // The continuation should still be classified as a teammate
    // because allSessions preserves the full session list.
    const root = makeSession({ id: "root", agent: "claude" });
    const teammate = makeSession({
      id: "tm",
      agent: "claude",
      parent_session_id: "root",
      first_message: "<teammate-message>hi</teammate-message>",
    });
    const cont = makeSession({
      id: "cont",
      agent: "claude",
      parent_session_id: "tm",
    });

    const fullSessions = [root, teammate, cont];
    const starred = new Set(["root", "cont"]); // teammate is NOT starred
    const filtered = fullSessions.filter((s) => starred.has(s.id));

    const group: SessionGroup = {
      key: "root",
      project: "test",
      sessions: filtered,
      allSessions: fullSessions, // full list for ancestry
      primarySessionId: "root",
      totalMessages: 30,
      firstMessage: "hi",
      startedAt: "2025-01-01T00:00:00Z",
      endedAt: "2025-01-01T01:00:00Z",
    };

    // Expand so children are emitted.
    const expanded = new Set(["root", `team:root`]);
    const items = buildDisplayItems(
      [group], [], "none", new Set(), expanded,
    );

    // cont should be classified as a teammate (under "Team"),
    // NOT as a continuation, because allSessions lets the
    // ancestry walk find the teammate parent.
    const teamHeader = items.find((i) => i.type === "team-group");
    expect(teamHeader).toBeDefined();
    expect(teamHeader!.label).toBe("Team");

    const contItem = items.find((i) => i.session?.id === "cont");
    expect(contItem).toBeDefined();
    expect(contItem!.depth).toBe(2);
  });

  it("recomputes primarySessionId using recency when original primary is unstarred", () => {
    // Root session s0 is the primary, children s1 and s2 are starred.
    // s2 is more recent so it should become the new primary.
    const root = makeSession({ id: "s0", agent: "claude" });
    const child1 = makeSession({
      id: "s1",
      agent: "claude",
      parent_session_id: "s0",
      ended_at: "2025-01-01T01:00:00Z",
    });
    const child2 = makeSession({
      id: "s2",
      agent: "claude",
      parent_session_id: "s0",
      ended_at: "2025-01-02T01:00:00Z",
    });
    const group: SessionGroup = {
      key: "s0",
      project: "test",
      sessions: [root, child1, child2],
      primarySessionId: "s0",
      totalMessages: 30,
      firstMessage: "hi",
      startedAt: "2025-01-01T00:00:00Z",
      endedAt: "2025-01-02T01:00:00Z",
    };

    // Only children are starred, not the root.
    const starred = new Set(["s1", "s2"]);
    const filtered = filterGroupsForStarred([group], starred);

    expect(filtered).toHaveLength(1);
    // primarySessionId must be the most recent surviving session.
    expect(filtered[0]!.primarySessionId).toBe("s2");
    expect(
      filtered[0]!.sessions.some(
        (s) => s.id === filtered[0]!.primarySessionId,
      ),
    ).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Child classification: subagent precedence over teammate
// ---------------------------------------------------------------------------

describe("child classification precedence", () => {
  it("subagent child of teammate is classified as subagent", () => {
    // root -> teammate -> subagent
    const root = makeSession({ id: "root", agent: "claude" });
    const teammate = makeSession({
      id: "tm",
      agent: "claude",
      parent_session_id: "root",
      first_message: "<teammate-message>hi</teammate-message>",
    });
    const subagent = makeSession({
      id: "sub",
      agent: "claude",
      parent_session_id: "tm",
      relationship_type: "subagent",
    });
    const group: SessionGroup = {
      key: "root",
      project: "test",
      sessions: [root, teammate, subagent],
      primarySessionId: "root",
      totalMessages: 30,
      firstMessage: "hi",
      startedAt: "2025-01-01T00:00:00Z",
      endedAt: "2025-01-01T01:00:00Z",
    };

    // Expand the group so children are emitted.
    const expanded = new Set([group.key]);
    const items = buildDisplayItems(
      [group], [], "none", new Set(), expanded,
    );

    // The subagent should appear under "Subagents", not "Team".
    const subagentHeader = items.find(
      (i) => i.type === "subagent-group",
    );
    expect(subagentHeader).toBeDefined();
    expect(subagentHeader!.label).toBe("Subagents");

    // The subagent session should be a child at depth 2 under
    // the subagent group (when expanded).
    const subKey = `subagent:${group.key}`;
    const expandedWithSub = new Set([group.key, subKey]);
    const items2 = buildDisplayItems(
      [group], [], "none", new Set(), expandedWithSub,
    );
    const subItem = items2.find(
      (i) => i.session?.id === "sub",
    );
    expect(subItem).toBeDefined();
    expect(subItem!.depth).toBe(2);

    // The teammate should be under "Team", not subagents.
    const teamHeader = items2.find(
      (i) => i.type === "team-group",
    );
    expect(teamHeader).toBeDefined();
    expect(teamHeader!.label).toBe("Team");
  });

  it("children are sorted most-recent-first within sub-groups", () => {
    const root = makeSession({ id: "root", agent: "claude" });
    const sub1 = makeSession({
      id: "sub-old",
      agent: "claude",
      parent_session_id: "root",
      relationship_type: "subagent",
      ended_at: "2025-01-01T01:00:00Z",
    });
    const sub2 = makeSession({
      id: "sub-mid",
      agent: "claude",
      parent_session_id: "root",
      relationship_type: "subagent",
      ended_at: "2025-01-02T01:00:00Z",
    });
    const sub3 = makeSession({
      id: "sub-new",
      agent: "claude",
      parent_session_id: "root",
      relationship_type: "subagent",
      ended_at: "2025-01-03T01:00:00Z",
    });
    const group: SessionGroup = {
      key: "root",
      project: "test",
      // Insert in ascending order (as buildSessionGroups does).
      sessions: [root, sub1, sub2, sub3],
      primarySessionId: "root",
      totalMessages: 40,
      firstMessage: "hi",
      startedAt: "2025-01-01T00:00:00Z",
      endedAt: "2025-01-03T01:00:00Z",
    };

    const expanded = new Set(["root", `subagent:root`]);
    const items = buildDisplayItems(
      [group], [], "none", new Set(), expanded,
    );

    const childItems = items.filter(
      (i) => i.session && i.depth === 2,
    );
    expect(childItems).toHaveLength(3);
    // Most recent first.
    expect(childItems[0]!.session!.id).toBe("sub-new");
    expect(childItems[1]!.session!.id).toBe("sub-mid");
    expect(childItems[2]!.session!.id).toBe("sub-old");
  });

  it("isSubagentDescendant returns true for child of subagent", () => {
    const subagent = makeSession({
      id: "sub",
      relationship_type: "subagent",
    });
    const child = makeSession({
      id: "child",
      parent_session_id: "sub",
    });
    expect(isSubagentDescendant(child, [subagent, child])).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// selectPrimaryId
// ---------------------------------------------------------------------------

describe("selectPrimaryId", () => {
  it("returns groupKey for empty sessions", () => {
    expect(selectPrimaryId([], "key")).toBe("key");
  });

  it("selects the most recent session by ended_at", () => {
    const old = makeSession({
      id: "old",
      ended_at: "2025-01-01T00:00:00Z",
    });
    const recent = makeSession({
      id: "recent",
      ended_at: "2025-01-03T00:00:00Z",
    });
    const mid = makeSession({
      id: "mid",
      ended_at: "2025-01-02T00:00:00Z",
    });
    expect(selectPrimaryId([old, mid, recent], "key")).toBe("recent");
  });

  it("prefers root session when group has subagents", () => {
    const root = makeSession({
      id: "root",
      ended_at: "2025-01-01T00:00:00Z",
    });
    const sub = makeSession({
      id: "sub",
      ended_at: "2025-01-05T00:00:00Z",
      relationship_type: "subagent",
    });
    // Despite sub being more recent, root should be chosen
    // because it matches the groupKey.
    expect(selectPrimaryId([root, sub], "root")).toBe("root");
  });

  it("falls back to first session when root is missing from subagent group", () => {
    const sub = makeSession({
      id: "sub",
      relationship_type: "subagent",
    });
    const other = makeSession({ id: "other" });
    expect(selectPrimaryId([sub, other], "missing")).toBe("sub");
  });

  it("uses started_at when ended_at is missing", () => {
    const a = makeSession({
      id: "a",
      started_at: "2025-01-01T00:00:00Z",
      ended_at: undefined,
    });
    const b = makeSession({
      id: "b",
      started_at: "2025-01-05T00:00:00Z",
      ended_at: undefined,
    });
    expect(selectPrimaryId([a, b], "key")).toBe("b");
  });
});
