import { describe, expect, it } from "vite-plus/test";
import type {
  DbSession,
  ServiceInputTreeNode,
  SessionTreeNode,
} from "../../api/generated/index.js";
import {
  inForkFamily,
  layoutInputTree,
  type InputTreeGapRow,
  type InputTreeNodeRow,
} from "./input-tree.js";

function inputNode(
  parent: number,
  extra: Partial<ServiceInputTreeNode> = {},
): ServiceInputTreeNode {
  return {
    parent,
    session_id: "s",
    ordinal: 0,
    preview: "input",
    is_shell: false,
    on_active_path: false,
    end_session_ids: [],
    ...extra,
  };
}

function nodeRows(rows: ReturnType<typeof layoutInputTree>): InputTreeNodeRow[] {
  return rows.filter((row): row is InputTreeNodeRow => row.kind === "node");
}

function shape(row: InputTreeNodeRow) {
  return {
    index: row.index,
    depth: row.depth,
    rails: row.rails,
    elbow: row.elbow,
    lineAbove: row.lineAbove,
    lineBelow: row.lineBelow,
  };
}

describe("layoutInputTree", () => {
  it("moves each branch one column right and keeps a rail beside earlier branches", () => {
    // 0
    // ├ 1 ─ 2
    // └ 3
    //   ├ 4
    //   └ 5
    const rows = layoutInputTree([
      inputNode(-1),
      inputNode(0),
      inputNode(1),
      inputNode(0),
      inputNode(3),
      inputNode(3),
    ]);

    expect(rows.map((row) => row.kind)).toEqual(Array(6).fill("node"));
    expect(nodeRows(rows).map(shape)).toEqual([
      { index: 0, depth: 0, rails: [], elbow: null, lineAbove: false, lineBelow: true },
      {
        index: 1,
        depth: 1,
        rails: [],
        elbow: { top: true, bottom: true },
        lineAbove: false,
        lineBelow: true,
      },
      { index: 2, depth: 1, rails: [0], elbow: null, lineAbove: true, lineBelow: false },
      {
        index: 3,
        depth: 1,
        rails: [],
        elbow: { top: true, bottom: false },
        lineAbove: false,
        lineBelow: true,
      },
      {
        index: 4,
        depth: 2,
        rails: [],
        elbow: { top: true, bottom: true },
        lineAbove: false,
        lineBelow: false,
      },
      {
        index: 5,
        depth: 2,
        rails: [],
        elbow: { top: true, bottom: false },
        lineAbove: false,
        lineBelow: false,
      },
    ]);
    const [root, , leaf, branchPoint] = nodeRows(rows);
    expect(root?.isBranchPoint).toBe(true);
    expect(leaf?.isLeaf).toBe(true);
    expect(branchPoint?.isBranchPoint).toBe(true);
  });

  it("branches separate top-level paths from an unseen root", () => {
    const rows = nodeRows(layoutInputTree([inputNode(-1), inputNode(-1)]));

    expect(rows.map(shape)).toEqual([
      {
        index: 0,
        depth: 1,
        rails: [],
        elbow: { top: false, bottom: true },
        lineAbove: false,
        lineBelow: false,
      },
      {
        index: 1,
        depth: 1,
        rails: [],
        elbow: { top: true, bottom: false },
        lineAbove: false,
        lineBelow: false,
      },
    ]);
  });

  it("marks the active session's path", () => {
    // The first input is on the path but outside the active session's
    // message view, as when the session continues another one.
    const rows = nodeRows(
      layoutInputTree([
        inputNode(-1, { on_active_path: true }),
        inputNode(0, { active_ordinal: 3 }),
        inputNode(0, { on_active_path: true, active_ordinal: 0 }),
      ]),
    );

    expect(rows.map((row) => row.onActivePath)).toEqual([true, false, true]);
    // A branch point's line continues down to every branch, so it is not
    // part of one path.
    expect(rows[0]?.lineBelowOnActivePath).toBe(false);
  });

  it("follows the active path down a single branch", () => {
    const rows = nodeRows(
      layoutInputTree([
        inputNode(-1, { on_active_path: true }),
        inputNode(0, { on_active_path: true }),
        inputNode(1),
      ]),
    );

    expect(rows.map((row) => row.lineBelowOnActivePath)).toEqual([true, false, false]);
  });

  describe("folding", () => {
    // 0 - 1 - 2 - 3 - 4 - 5, then 5 splits into 6 and 7.
    const chain = [
      inputNode(-1),
      inputNode(0),
      inputNode(1, { on_active_path: true }),
      inputNode(2),
      inputNode(3),
      inputNode(4),
      inputNode(5),
      inputNode(5),
    ];

    it("folds the inputs between a branch's first and last visible input", () => {
      const rows = layoutInputTree(chain);

      expect(rows.map((row) => row.key)).toEqual(["n0", "g1", "n5", "n6", "n7"]);
      const gap = rows[1] as InputTreeGapRow;
      expect(gap).toMatchObject({
        runStart: 1,
        hidden: 4,
        depth: 0,
        rails: [],
        onActivePath: true,
      });
    });

    it("shows an unfolded run", () => {
      const rows = layoutInputTree(chain, { expandedRuns: new Set([1]) });

      expect(rows.map((row) => row.key)).toEqual(["n0", "n1", "n2", "n3", "n4", "n5", "n6", "n7"]);
    });

    it("keeps a run open while it holds an input that must stay visible", () => {
      const rows = layoutInputTree(chain, { keepVisible: new Set([3]) });

      expect(rows.some((row) => row.kind === "gap")).toBe(false);
    });

    it("keeps an input where a session ends visible while another path continues below it", () => {
      // 0 - 1 - 2 - 3 - 4 ends one session - 5 - 6 - 7 - 8 ends the one
      // that continued it.
      const rows = layoutInputTree([
        inputNode(-1),
        inputNode(0),
        inputNode(1),
        inputNode(2),
        inputNode(3, { end_session_ids: ["parent"] }),
        inputNode(4),
        inputNode(5),
        inputNode(6),
        inputNode(7, { end_session_ids: ["continued"] }),
      ]);

      expect(rows.map((row) => row.key)).toEqual(["n0", "g1", "n4", "g5", "n8"]);
      expect(rows[1]).toMatchObject({ kind: "gap", runStart: 1, hidden: 3 });
      expect(rows[3]).toMatchObject({ kind: "gap", runStart: 5, hidden: 3 });
    });

    it("leaves short runs alone", () => {
      const rows = layoutInputTree([inputNode(-1), inputNode(0), inputNode(1), inputNode(2)]);

      expect(rows.map((row) => row.key)).toEqual(["n0", "n1", "n2", "n3"]);
    });
  });
});

function session(id: string, relationship?: string): DbSession {
  return { id, relationship_type: relationship } as DbSession;
}

function treeNode(
  id: string,
  relationship?: string,
  children: SessionTreeNode[] = [],
): SessionTreeNode {
  return {
    session: session(id, relationship),
    children,
    depth: 0,
    is_active: false,
    is_branch_start: false,
    is_leaf: children.length === 0,
  };
}

describe("inForkFamily", () => {
  // root
  // ├ fork-a (fork)
  // │ └ fork-b (fork)
  // └ helper (subagent)
  //   └ fork-c (fork)
  const forks = treeNode("root", undefined, [
    treeNode("fork-a", "fork", [treeNode("fork-b", "fork")]),
    treeNode("helper", "subagent", [treeNode("fork-c", "fork")]),
  ]);

  it.each(["root", "fork-a", "fork-b", "helper", "fork-c"])(
    "finds the family of %s",
    (activeId) => {
      expect(inForkFamily(forks, activeId)).toBe(true);
    },
  );

  it("finds no family for a session that is missing from the tree", () => {
    expect(inForkFamily(forks, "missing")).toBe(false);
  });

  it("finds no family for a session on its own", () => {
    expect(inForkFamily(treeNode("solo"), "solo")).toBe(false);
  });

  // base
  // ├ main (fork)
  // │ └ moved (continuation)
  // │   └ helper (subagent)
  // └ side (fork)
  const continued = treeNode("base", undefined, [
    treeNode("main", "fork", [
      treeNode("moved", "continuation", [treeNode("helper", "subagent")]),
    ]),
    treeNode("side", "fork"),
  ]);

  it("finds the family of a continuation of a fork", () => {
    expect(inForkFamily(continued, "moved")).toBe(true);
  });

  it("leaves a sub-agent of a continuation out of the family", () => {
    expect(inForkFamily(continued, "helper")).toBe(false);
  });

  it("finds the family of every session in a chain of continuations", () => {
    // first ─ second (continuation) ─ third (continuation)
    const chain = treeNode("first", undefined, [
      treeNode("second", "continuation", [treeNode("third", "continuation")]),
    ]);

    for (const activeId of ["first", "second", "third"]) {
      expect(inForkFamily(chain, activeId), activeId).toBe(true);
    }
  });

  it("keeps a session out of the family its sub-agent starts", () => {
    // root ─ helper (subagent) ─ moved (continuation)
    const tree = treeNode("root", undefined, [
      treeNode("helper", "subagent", [treeNode("moved", "continuation")]),
    ]);

    expect(inForkFamily(tree, "root")).toBe(false);
    expect(inForkFamily(tree, "helper")).toBe(true);
    expect(inForkFamily(tree, "moved")).toBe(true);
  });
});
