import { describe, expect, it } from "vite-plus/test";
import type { ServiceInputTreeNode } from "../../api/generated/index.js";
import {
  inputGraphPathTo,
  layoutInputGraph,
  type InputGraphItem,
  type InputGraphLayout,
} from "./input-graph.js";

function inputNode(
  parent: number,
  extra: Partial<ServiceInputTreeNode> = {},
): ServiceInputTreeNode {
  return {
    parent,
    session_id: "root",
    ordinal: 0,
    preview: "input",
    is_shell: false,
    end_session_ids: [],
    ...extra,
  };
}

function item(layout: InputGraphLayout, key: string): InputGraphItem {
  const found = layout.items.find((candidate) => candidate.key === key);
  expect(found, key).toBeDefined();
  return found!;
}

function centerX(entry: InputGraphItem): number {
  return entry.x + entry.width / 2;
}

// 0 ─ 1 ─ 2 ─ 3 ─ 4 ─┬ 5 ends root
//                    └ 6 ends child
function longRun(extra: Partial<ServiceInputTreeNode> = {}): ServiceInputTreeNode[] {
  return [
    inputNode(-1),
    inputNode(0, extra),
    inputNode(1, extra),
    inputNode(2, extra),
    inputNode(3, extra),
    inputNode(4, { end_session_ids: ["root"] }),
    inputNode(4, { session_id: "child", end_session_ids: ["child"] }),
  ];
}

describe("layoutInputGraph", () => {
  it("hangs each session below the input it ends at, so every leaf is a session", () => {
    // 0 ─┬ 1 ends root
    //    └ 2 ends child
    const layout = layoutInputGraph(
      [
        inputNode(-1, { active_ordinal: -5 }),
        inputNode(0, { end_session_ids: ["root"] }),
        inputNode(0, {
          session_id: "child",
          active_ordinal: 0,
          end_session_ids: ["child"],
        }),
      ],
      { activeSessionId: "child", sessionOrder: ["root", "child"] },
    );

    expect(layout.items.map((entry) => [entry.key, entry.kind, entry.parentKey])).toEqual([
      ["n0", "input", null],
      ["n1", "input", "n0"],
      ["s1:root", "session", "n1"],
      ["n2", "input", "n0"],
      ["s2:child", "session", "n2"],
    ]);
    // One row per depth, one column per leaf, the parent centred above.
    expect(layout.items.map((entry) => [entry.key, entry.x, entry.y])).toEqual([
      ["n0", 168, 32],
      ["n1", 32, 136],
      ["s1:root", 32, 240],
      ["n2", 304, 136],
      ["s2:child", 304, 240],
    ]);
    expect(layout.width).toBe(576);
    expect(layout.height).toBe(336);
    expect(layout.items.filter((entry) => entry.onActivePath).map((entry) => entry.key)).toEqual([
      "n0",
      "n2",
      "s2:child",
    ]);
  });

  it("draws edges that split below the parent and puts the active path last", () => {
    const layout = layoutInputGraph(
      [
        inputNode(-1, { active_ordinal: 0 }),
        inputNode(0, { active_ordinal: 2, end_session_ids: ["root"] }),
        inputNode(0, { session_id: "child", end_session_ids: ["child"] }),
      ],
      { activeSessionId: "root", sessionOrder: ["root", "child"] },
    );

    expect(layout.edges.map((edge) => [edge.key, edge.onActivePath])).toEqual([
      ["n0>n2", false],
      ["n2>s2:child", false],
      ["n0>n1", true],
      ["n1>s1:root", true],
    ]);
    const edges = new Map(layout.edges.map((edge) => [edge.key, edge.d]));
    // Down from the parent's bottom edge, across 16px below it with rounded
    // corners, and down into the child's top edge.
    expect(edges.get("n0>n1")).toBe("M288,96V104Q288,112 280,112H160Q152,112 152,120V136");
    expect(edges.get("n0>n2")).toBe("M288,96V104Q288,112 296,112H416Q424,112 424,120V136");
    expect(edges.get("n1>s1:root")).toBe("M152,200V240");
  });

  it("orders siblings by the earliest session that passes through them", () => {
    // Input 1 belongs to a session that started after the one holding 2.
    const layout = layoutInputGraph(
      [
        inputNode(-1),
        inputNode(0, { session_id: "late", end_session_ids: ["late"] }),
        inputNode(0, { session_id: "early" }),
        inputNode(2, { session_id: "early", end_session_ids: ["early"] }),
      ],
      { activeSessionId: "early", sessionOrder: ["root", "early", "late"] },
    );

    expect(layout.items.map((entry) => entry.key)).toEqual([
      "n0",
      "n2",
      "n3",
      "s3:early",
      "n1",
      "s1:late",
    ]);
    expect(centerX(item(layout, "n2"))).toBeLessThan(centerX(item(layout, "n1")));
  });

  it("splits an input that ends one session and continues in another", () => {
    // 0 ─ 1 ends root ─ 2 ends fork
    const layout = layoutInputGraph(
      [
        inputNode(-1),
        inputNode(0, { end_session_ids: ["root"] }),
        inputNode(1, { session_id: "fork", end_session_ids: ["fork"] }),
      ],
      { activeSessionId: "fork", sessionOrder: ["root", "fork"] },
    );

    expect(item(layout, "n1")).toMatchObject({ kind: "input", branches: 2 });
    expect(item(layout, "n0")).toMatchObject({ kind: "input", branches: 1 });
    expect(centerX(item(layout, "s1:root"))).toBeLessThan(centerX(item(layout, "n2")));
  });

  it("folds a run of inputs that neither start, end, nor split a branch", () => {
    const layout = layoutInputGraph(longRun({ active_ordinal: 3 }), {
      activeSessionId: "child",
      sessionOrder: ["root", "child"],
    });

    expect(layout.items.map((entry) => [entry.key, entry.parentKey])).toEqual([
      ["n0", null],
      ["r1", "n0"],
      ["n4", "r1"],
      ["n5", "n4"],
      ["s5:root", "n5"],
      ["n6", "n4"],
      ["s6:child", "n6"],
    ]);
    // Input 4 splits, so the run holds inputs 1 to 3.
    const run = item(layout, "r1");
    expect(run).toMatchObject({ kind: "run", runStart: 1, hidden: 3, onActivePath: true });
    // The small run card sits centred in its row.
    expect(run.height).toBe(30);
    expect(run.y).toBe(153);
  });

  it("shows every input of a run that is unfolded, pinned, or folding is off", () => {
    const options = { activeSessionId: "child", sessionOrder: ["root", "child"] };
    const allInputs = ["n0", "n1", "n2", "n3", "n4", "n5", "n6"];
    const inputKeys = (layout: InputGraphLayout) =>
      layout.items.filter((entry) => entry.kind === "input").map((entry) => entry.key);

    expect(inputKeys(layoutInputGraph(longRun(), { ...options, fold: false }))).toEqual(allInputs);
    expect(
      inputKeys(layoutInputGraph(longRun(), { ...options, expandedRuns: new Set([1]) })),
    ).toEqual(allInputs);
    expect(
      inputKeys(layoutInputGraph(longRun(), { ...options, keepVisible: new Set([3]) })),
    ).toEqual(allInputs);
  });

  it("keeps the first input of a branch and short runs visible", () => {
    // 0 ─┬ 1 ─ 2 ─ 3 ─ 4 ─ 5 ends a
    //    └ 6 ─ 7 ─ 8 ends b
    const layout = layoutInputGraph(
      [
        inputNode(-1),
        inputNode(0, { session_id: "a" }),
        inputNode(1, { session_id: "a" }),
        inputNode(2, { session_id: "a" }),
        inputNode(3, { session_id: "a" }),
        inputNode(4, { session_id: "a", end_session_ids: ["a"] }),
        inputNode(0, { session_id: "b" }),
        inputNode(6, { session_id: "b" }),
        inputNode(7, { session_id: "b", end_session_ids: ["b"] }),
      ],
      { activeSessionId: "a", sessionOrder: ["a", "b"] },
    );

    expect(layout.items.map((entry) => entry.key)).toEqual([
      "n0",
      "n1",
      "r2",
      "n5",
      "s5:a",
      "n6",
      "n7",
      "n8",
      "s8:b",
    ]);
    expect(item(layout, "r2")).toMatchObject({ hidden: 3, sessionId: "a" });
  });

  it("places separate top-level paths side by side without a drawn root", () => {
    const layout = layoutInputGraph(
      [
        inputNode(-1, { end_session_ids: ["a"] }),
        inputNode(-1, { session_id: "b", end_session_ids: ["b"] }),
      ],
      { activeSessionId: "a", sessionOrder: ["a", "b"] },
    );

    expect(layout.items.map((entry) => [entry.key, entry.parentKey, entry.y])).toEqual([
      ["n0", null, 32],
      ["s0:a", "n0", 136],
      ["n1", null, 32],
      ["s1:b", "n1", 136],
    ]);
    expect(layout.edges.map((edge) => edge.key)).toEqual(["n1>s1:b", "n0>s0:a"]);
  });

  it("returns an empty layout without inputs", () => {
    expect(layoutInputGraph([], { activeSessionId: "a" })).toEqual({
      items: [],
      edges: [],
      width: 0,
      height: 0,
    });
  });
});

describe("inputGraphPathTo", () => {
  it("collects an item and everything above it", () => {
    const layout = layoutInputGraph(longRun(), {
      activeSessionId: "child",
      sessionOrder: ["root", "child"],
    });

    expect([...inputGraphPathTo(layout, "s6:child")]).toEqual(["s6:child", "n6", "n4", "r1", "n0"]);
    expect(inputGraphPathTo(layout, null).size).toBe(0);
  });
});
