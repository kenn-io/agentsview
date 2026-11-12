import { describe, expect, it } from "vite-plus/test";
import type { DbRateLimitSnapshot } from "../api/generated/index.js";
import { rateLimitGroups, rateLimitWindowLabel, type RateLimitGroup } from "./rateLimits.js";

const NOW = Date.parse("2026-11-12T12:00:00Z");

function onlyGroup(snapshots: DbRateLimitSnapshot[]): RateLimitGroup {
  const groups = rateLimitGroups(snapshots, NOW);
  expect(groups).toHaveLength(1);
  return groups[0] as RateLimitGroup;
}

function windowAt(group: RateLimitGroup, index: number) {
  const row = group.windows[index];
  if (!row) throw new Error(`no window ${index}`);
  return row;
}

function snapshot(overrides: Partial<DbRateLimitSnapshot> = {}): DbRateLimitSnapshot {
  return {
    agent: "codex",
    account_key: "a",
    limit_id: "codex",
    plan_type: "pro",
    session_id: "codex:s1",
    observed_at: "2026-11-12T11:00:00.000Z",
    primary: { used_percent: 12, window_minutes: 10080, resets_at: "2026-11-17T04:43:21.000Z" },
    ...overrides,
  };
}

describe("rateLimitWindowLabel", () => {
  it.each([
    [300, "5-hour"],
    [10080, "Weekly"],
    [2880, "2-day"],
    [90, "90-minute"],
  ])("names a %d-minute window %s", (minutes, label) => {
    expect(rateLimitWindowLabel(minutes, "primary")).toBe(label);
  });

  it("falls back to the slot when the agent reported no usable length", () => {
    expect(rateLimitWindowLabel(undefined, "primary")).toBe("Primary window");
    expect(rateLimitWindowLabel(0, "secondary")).toBe("Secondary window");
  });
});

describe("rateLimitGroups", () => {
  it("describes each reported window", () => {
    const group = onlyGroup([
      snapshot({
        primary: { used_percent: 40.5, window_minutes: 300, resets_at: "2026-11-12T15:00:00.000Z" },
        secondary: { used_percent: 12, window_minutes: 10080 },
      }),
    ]);
    expect(group.title).toBe("Codex · pro");
    expect(group.windows.map((w) => [w.label, w.usedText, w.resetSince])).toEqual([
      ["5-hour", "40.5% used", false],
      ["Weekly", "12% used", false],
    ]);
    expect(windowAt(group, 0).resetText).toMatch(/^resets /);
    // No reset time was reported for the weekly window.
    expect(windowAt(group, 1).resetText).toBe("");
  });

  it("says when a window reset after the report", () => {
    const group = onlyGroup([
      snapshot({
        primary: { used_percent: 97, window_minutes: 300, resets_at: "2026-11-12T11:30:00.000Z" },
      }),
    ]);
    expect(windowAt(group, 0).resetSince).toBe(true);
    expect(windowAt(group, 0).resetText).toBe("reset since this report");
  });

  it("clamps the meter but keeps the reported share in the text", () => {
    const group = onlyGroup([snapshot({ primary: { used_percent: 104, window_minutes: 300 } })]);
    expect(windowAt(group, 0).percent).toBe(100);
    expect(windowAt(group, 0).usedText).toBe("104% used");
  });

  it("numbers accounts only when one agent reports several", () => {
    const groups = rateLimitGroups(
      [snapshot({ account_key: "a" }), snapshot({ account_key: "b", plan_type: "plus" })],
      NOW,
    );
    expect(groups.map((g) => g.title)).toEqual([
      "Codex · pro · account 1",
      "Codex · plus · account 2",
    ]);
    // A newer report from account b moves it first without renumbering.
    const reordered = rateLimitGroups(
      [snapshot({ account_key: "b", plan_type: "plus" }), snapshot({ account_key: "a" })],
      NOW,
    );
    expect(reordered.map((g) => g.title)).toEqual([
      "Codex · plus · account 2",
      "Codex · pro · account 1",
    ]);
  });

  it("names a limit other than the agent's own", () => {
    const group = onlyGroup([snapshot({ limit_id: "premium", plan_type: "" })]);
    expect(group.title).toBe("Codex · premium");
  });
});
