import type {
  ServiceInputTreeNode as InputTreeNode,
  SessionTreeNode,
} from "../../api/generated/index.js";

// A run of inputs between two visible ones folds into a single row once it
// holds at least this many inputs.
export const INPUT_TREE_COLLAPSE_MIN = 3;

// Connector drawn from the column to the left of a row that starts a branch.
export interface InputTreeElbow {
  // The connector comes down from the row above.
  top: boolean;
  // More branches start below, so the connector continues down.
  bottom: boolean;
}

export interface InputTreeNodeRow {
  kind: "node";
  key: string;
  index: number;
  node: InputTreeNode;
  // Column of the row's marker: the number of branches above it.
  depth: number;
  // Columns left of the marker with a line through the whole row.
  rails: number[];
  elbow: InputTreeElbow | null;
  // Lines in the marker column, linking the row to the input before and
  // after it on the same branch.
  lineAbove: boolean;
  lineBelow: boolean;
  lineBelowOnActivePath: boolean;
  isLeaf: boolean;
  isBranchPoint: boolean;
  onActivePath: boolean;
}

// Stands in for a folded run of inputs.
export interface InputTreeGapRow {
  kind: "gap";
  key: string;
  // Index of the first folded input, which identifies the run.
  runStart: number;
  hidden: number;
  depth: number;
  rails: number[];
  onActivePath: boolean;
}

export type InputTreeRow = InputTreeNodeRow | InputTreeGapRow;

export interface InputTreeLayoutOptions {
  // Runs, by their first index, the reader unfolded.
  expandedRuns?: ReadonlySet<number>;
  // Inputs that must stay visible; runs that hold one stay unfolded.
  keepVisible?: ReadonlySet<number>;
}

// Lays out input tree nodes, which arrive depth first with parents before
// children, as rows with the lines that connect them. A branch's inputs
// share one column, and each branch moves one column to the right of the
// input it branched from.
export function layoutInputTree(
  nodes: readonly InputTreeNode[],
  options: InputTreeLayoutOptions = {},
): InputTreeRow[] {
  const children: number[][] = nodes.map(() => []);
  const topLevel: number[] = [];
  const parents = nodes.map((node, index) => {
    // A parent must come first; treat anything else as a new path.
    if (node.parent >= 0 && node.parent < index) {
      children[node.parent]!.push(index);
      return node.parent;
    }
    topLevel.push(index);
    return -1;
  });

  const depth: number[] = [];
  const childRails: number[][] = [];
  const rows: InputTreeNodeRow[] = [];
  nodes.forEach((node, index) => {
    const parent = parents[index]!;
    const siblings = parent < 0 ? topLevel : children[parent]!;
    const isHead = siblings.length > 1;
    const isFirst = siblings[0] === index;
    const isLast = siblings[siblings.length - 1] === index;
    // Separate top-level paths branch from an unseen root in column 0.
    const parentDepth = parent < 0 ? 0 : depth[parent]!;
    depth[index] = isHead ? parentDepth + 1 : parentDepth;

    const rails = parent < 0 ? [] : childRails[parent]!;
    childRails[index] = isHead && !isLast ? [...rails, depth[index]! - 1] : rails;

    const kids = children[index]!;
    rows.push({
      kind: "node",
      key: `n${index}`,
      index,
      node,
      depth: depth[index]!,
      rails,
      elbow: isHead ? { top: !(parent < 0 && isFirst), bottom: !isLast } : null,
      lineAbove: !isHead && parent >= 0,
      lineBelow: kids.length > 0,
      lineBelowOnActivePath: kids.length === 1 && isOnActivePath(nodes[kids[0]!]!),
      isLeaf: kids.length === 0,
      isBranchPoint: kids.length > 1,
      onActivePath: isOnActivePath(node),
    });
  });

  return foldRuns(rows, options);
}

function isOnActivePath(node: InputTreeNode): boolean {
  return node.on_active_path;
}

// An input is foldable when the inputs before and after it on its branch
// are its only neighbours: it neither starts, ends, nor splits a branch.
// An input where a session ends stays visible to show that session's label,
// even when another session's path carries on below it.
function isFoldable(row: InputTreeNodeRow): boolean {
  return (
    row.lineAbove &&
    !row.isLeaf &&
    !row.isBranchPoint &&
    row.node.end_session_ids.length === 0
  );
}

function foldRuns(
  rows: InputTreeNodeRow[],
  { expandedRuns, keepVisible }: InputTreeLayoutOptions,
): InputTreeRow[] {
  const out: InputTreeRow[] = [];
  let i = 0;
  while (i < rows.length) {
    const row = rows[i]!;
    if (!isFoldable(row)) {
      out.push(row);
      i++;
      continue;
    }
    // A foldable input's only child follows it, so a run is contiguous.
    let end = i;
    while (end + 1 < rows.length && isFoldable(rows[end + 1]!)) end++;
    const run = rows.slice(i, end + 1);
    const pinned = run.some((r) => keepVisible?.has(r.index));
    if (run.length < INPUT_TREE_COLLAPSE_MIN || pinned || expandedRuns?.has(row.index)) {
      out.push(...run);
    } else {
      out.push({
        kind: "gap",
        key: `g${row.index}`,
        runStart: row.index,
        hidden: run.length,
        depth: row.depth,
        rails: row.rails,
        onActivePath: run.some((r) => r.onActivePath),
      });
    }
    i = end + 1;
  }
  return out;
}

// Fork families are linked by forks, which branch from their parent's
// inputs, and continuations, which carry them on.
function isFamilyLink(node: SessionTreeNode): boolean {
  const relationship = node.session.relationship_type;
  return relationship === "fork" || relationship === "continuation";
}

// Reports whether the active session's fork family holds other sessions:
// a fork or continuation links the session to its parent or to one of its
// children. A family linked by continuations alone does not branch, but its
// inputs still span every session in it.
export function inForkFamily(root: SessionTreeNode, activeId: string): boolean {
  if (root.session.id === activeId) return root.children.some(isFamilyLink);
  const stack = [root];
  while (stack.length > 0) {
    const node = stack.pop()!;
    for (const child of node.children) {
      if (child.session.id === activeId) {
        return isFamilyLink(child) || child.children.some(isFamilyLink);
      }
      stack.push(child);
    }
  }
  return false;
}
