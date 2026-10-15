import { hierarchy, tree } from "d3-hierarchy";
import type { ServiceInputTreeNode as InputTreeNode } from "../../api/generated/index.js";
import { INPUT_TREE_COLLAPSE_MIN } from "./input-tree.js";

// Sizes of the input graph in CSS pixels at 100% zoom. Every card shares one
// width, and each depth of the tree is one row.
export const INPUT_GRAPH_NODE_WIDTH = 240;
export const INPUT_GRAPH_CARD_HEIGHT = 64;
export const INPUT_GRAPH_RUN_HEIGHT = 30;
export const INPUT_GRAPH_ROW_PITCH = 104;
export const INPUT_GRAPH_PADDING = 32;
const COLUMN_GAP = 32;
// Edges leave a parent's bottom edge, split this far below it, and drop into
// each child's top edge.
const SPLIT_OFFSET = 16;
const CORNER_RADIUS = 8;

interface InputGraphItemBase {
  key: string;
  // Top-left corner and size of the item's card.
  x: number;
  y: number;
  width: number;
  height: number;
  // Key of the item drawn above this one, or null at the top of the graph.
  parentKey: string | null;
  onActivePath: boolean;
}

// One input in its own card.
export interface InputGraphInputItem extends InputGraphItemBase {
  kind: "input";
  index: number;
  node: InputTreeNode;
  // Items drawn below the input: the inputs that follow it and the sessions
  // that end at it.
  branches: number;
}

// Stands in for a folded run of inputs.
export interface InputGraphRunItem extends InputGraphItemBase {
  kind: "run";
  // Index of the first folded input, which identifies the run.
  runStart: number;
  hidden: number;
  // The session that holds the first folded input.
  sessionId: string;
}

// A session whose path ends at the input above it. Every leaf of the graph
// is a session.
export interface InputGraphSessionItem extends InputGraphItemBase {
  kind: "session";
  sessionId: string;
  // Index of the input the session ends at.
  endIndex: number;
}

export type InputGraphItem = InputGraphInputItem | InputGraphRunItem | InputGraphSessionItem;

export interface InputGraphEdge {
  key: string;
  from: string;
  to: string;
  // SVG path data in graph coordinates.
  d: string;
  onActivePath: boolean;
}

export interface InputGraphLayout {
  // Items in depth-first order, parents before children.
  items: InputGraphItem[];
  // Edges off the active path come first, so the path draws on top.
  edges: InputGraphEdge[];
  width: number;
  height: number;
}

export interface InputGraphOptions {
  activeSessionId: string;
  // Session ids in the order they started. Siblings are ordered by the
  // earliest session that passes through them.
  sessionOrder?: readonly string[];
  // Fold runs of inputs that neither start, end, nor split a branch.
  fold?: boolean;
  // Runs, by their first index, the reader unfolded.
  expandedRuns?: ReadonlySet<number>;
  // Inputs that must stay visible; runs that hold one stay unfolded.
  keepVisible?: ReadonlySet<number>;
}

// An item drawn below an input: the next input, or a session that ends there.
type Entry = { input: number } | { session: string };

// An item before layout gives it a position.
type Draft =
  | (Omit<InputGraphInputItem, keyof InputGraphItemBase | "branches"> & DraftBase)
  | (Omit<InputGraphRunItem, keyof InputGraphItemBase> & DraftBase)
  | (Omit<InputGraphSessionItem, keyof InputGraphItemBase> & DraftBase)
  | ({ kind: "root" } & DraftBase);

interface DraftBase {
  key: string;
  onActivePath: boolean;
  children: Draft[];
}

function isOnActivePath(node: InputTreeNode): boolean {
  return node.active_ordinal !== undefined && node.active_ordinal !== null;
}

// Lays out input tree nodes, which arrive depth first with parents before
// children, as a top-down graph. Inputs are cards, each session that ends at
// an input hangs below it as a leaf card, and runs of inputs without a split
// can fold into one small card.
export function layoutInputGraph(
  nodes: readonly InputTreeNode[],
  options: InputGraphOptions,
): InputGraphLayout {
  const { activeSessionId, sessionOrder = [], fold = true, expandedRuns, keepVisible } = options;
  if (nodes.length === 0) return { items: [], edges: [], width: 0, height: 0 };

  const rank = new Map(sessionOrder.map((id, i) => [id, i]));
  const rankOf = (id: string) => rank.get(id) ?? sessionOrder.length;

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

  // The earliest session whose path runs through each input. Children come
  // after their parents, so a reverse pass sees every child first.
  const firstRank = nodes.map((node) =>
    node.end_session_ids.reduce((min, id) => Math.min(min, rankOf(id)), Infinity),
  );
  for (let i = nodes.length - 1; i >= 0; i--) {
    const parent = parents[i]!;
    if (parent >= 0) firstRank[parent] = Math.min(firstRank[parent]!, firstRank[i]!);
  }

  const entries: Entry[][] = nodes.map((node, index) =>
    [
      ...children[index]!.map((child) => ({ entry: { input: child }, rank: firstRank[child]! })),
      ...node.end_session_ids.map((id) => ({ entry: { session: id }, rank: rankOf(id) })),
    ]
      .sort((a, b) => a.rank - b.rank)
      .map(({ entry }) => entry as Entry),
  );
  const onlyInputChild = (index: number): number | null => {
    const list = entries[index]!;
    const first = list[0];
    return list.length === 1 && first && "input" in first ? first.input : null;
  };

  // An input folds when it is the only item below its parent and an input is
  // the only item below it: it neither starts, ends, nor splits a branch.
  const foldable = nodes.map((_, index) => {
    const parent = parents[index]!;
    return fold && parent >= 0 && entries[parent]!.length === 1 && onlyInputChild(index) !== null;
  });

  const runs = new Map<number, number[]>();
  const folded = nodes.map(() => false);
  nodes.forEach((_, index) => {
    if (!foldable[index] || foldable[parents[index]!]) return;
    const run = [index];
    for (let next = onlyInputChild(index)!; foldable[next]; next = onlyInputChild(next)!) {
      run.push(next);
    }
    const pinned = run.some((member) => keepVisible?.has(member));
    if (run.length < INPUT_TREE_COLLAPSE_MIN || pinned || expandedRuns?.has(index)) return;
    for (const member of run) folded[member] = true;
    runs.set(index, run);
  });

  // A folded input below a visible one always starts a run, so drafts holds
  // the card for every input a visible item links to.
  const drafts: Draft[] = [];
  for (const [start, run] of runs) {
    drafts[start] = {
      kind: "run",
      key: `r${start}`,
      runStart: start,
      hidden: run.length,
      sessionId: nodes[start]!.session_id,
      onActivePath: run.some((member) => isOnActivePath(nodes[member]!)),
      children: [],
    };
  }
  nodes.forEach((node, index) => {
    if (folded[index]) return;
    drafts[index] = {
      kind: "input",
      key: `n${index}`,
      index,
      node,
      onActivePath: isOnActivePath(node),
      children: [],
    };
  });
  nodes.forEach((_, index) => {
    if (folded[index]) return;
    drafts[index]!.children = entries[index]!.map((entry): Draft =>
      "input" in entry
        ? drafts[entry.input]!
        : {
            kind: "session",
            key: `s${index}:${entry.session}`,
            sessionId: entry.session,
            endIndex: index,
            onActivePath: entry.session === activeSessionId,
            children: [],
          },
    );
  });
  for (const [start, run] of runs) {
    drafts[start]!.children = [drafts[onlyInputChild(run[run.length - 1]!)!]!];
  }

  const tops = topLevel
    .map((index) => ({ index, rank: firstRank[index]! }))
    .sort((a, b) => a.rank - b.rank)
    .map(({ index }) => drafts[index]!);
  // Separate top-level paths hang from a root that is not drawn.
  const root: Draft =
    tops.length === 1
      ? tops[0]!
      : { kind: "root", key: "root", onActivePath: false, children: tops };
  return placeDrafts(root);
}

function placeDrafts(root: Draft): InputGraphLayout {
  const laidOut = tree<Draft>()
    .nodeSize([INPUT_GRAPH_NODE_WIDTH + COLUMN_GAP, INPUT_GRAPH_ROW_PITCH])
    .separation(() => 1)(hierarchy(root, (draft) => draft.children));

  const topDepth = root.kind === "root" ? 1 : 0;
  let minX = Infinity;
  let maxX = -Infinity;
  let maxDepth = topDepth;
  laidOut.each((point) => {
    if (point.data.kind === "root") return;
    minX = Math.min(minX, point.x);
    maxX = Math.max(maxX, point.x);
    maxDepth = Math.max(maxDepth, point.depth);
  });

  const items: InputGraphItem[] = [];
  const byKey = new Map<string, InputGraphItem>();
  laidOut.eachBefore((point) => {
    const { data } = point;
    if (data.kind === "root") return;
    const height = data.kind === "run" ? INPUT_GRAPH_RUN_HEIGHT : INPUT_GRAPH_CARD_HEIGHT;
    const centerX = point.x - minX + INPUT_GRAPH_PADDING + INPUT_GRAPH_NODE_WIDTH / 2;
    const centerY =
      (point.depth - topDepth) * INPUT_GRAPH_ROW_PITCH +
      INPUT_GRAPH_PADDING +
      INPUT_GRAPH_CARD_HEIGHT / 2;
    const parent = point.parent?.data.kind === "root" ? null : point.parent?.data.key;
    const base: InputGraphItemBase = {
      key: data.key,
      x: centerX - INPUT_GRAPH_NODE_WIDTH / 2,
      y: centerY - height / 2,
      width: INPUT_GRAPH_NODE_WIDTH,
      height,
      parentKey: parent ?? null,
      onActivePath: data.onActivePath,
    };
    let item: InputGraphItem;
    switch (data.kind) {
      case "input":
        item = {
          ...base,
          kind: "input",
          index: data.index,
          node: data.node,
          branches: data.children.length,
        };
        break;
      case "run":
        item = {
          ...base,
          kind: "run",
          runStart: data.runStart,
          hidden: data.hidden,
          sessionId: data.sessionId,
        };
        break;
      case "session":
        item = { ...base, kind: "session", sessionId: data.sessionId, endIndex: data.endIndex };
        break;
    }
    items.push(item);
    byKey.set(item.key, item);
  });

  const edges: InputGraphEdge[] = [];
  for (const item of items) {
    const parent = item.parentKey === null ? undefined : byKey.get(item.parentKey);
    if (!parent) continue;
    const x1 = parent.x + parent.width / 2;
    const y1 = parent.y + parent.height;
    edges.push({
      key: `${parent.key}>${item.key}`,
      from: parent.key,
      to: item.key,
      d: edgePath(x1, y1, item.x + item.width / 2, item.y, y1 + SPLIT_OFFSET),
      onActivePath: item.onActivePath,
    });
  }
  edges.sort((a, b) => Number(a.onActivePath) - Number(b.onActivePath));

  return {
    items,
    edges,
    width: maxX - minX + INPUT_GRAPH_NODE_WIDTH + 2 * INPUT_GRAPH_PADDING,
    height:
      (maxDepth - topDepth) * INPUT_GRAPH_ROW_PITCH +
      INPUT_GRAPH_CARD_HEIGHT +
      2 * INPUT_GRAPH_PADDING,
  };
}

function round(value: number): number {
  return Math.round(value * 10) / 10;
}

// A path down from (x1, y1), across at splitY with rounded corners, and down
// into (x2, y2).
function edgePath(x1: number, y1: number, x2: number, y2: number, splitY: number): string {
  if (Math.abs(x2 - x1) < 0.5) return `M${round(x1)},${round(y1)}V${round(y2)}`;
  const dir = x2 > x1 ? 1 : -1;
  const r = Math.max(0, Math.min(CORNER_RADIUS, Math.abs(x2 - x1) / 2, splitY - y1, y2 - splitY));
  return [
    `M${round(x1)},${round(y1)}`,
    `V${round(splitY - r)}`,
    `Q${round(x1)},${round(splitY)} ${round(x1 + dir * r)},${round(splitY)}`,
    `H${round(x2 - dir * r)}`,
    `Q${round(x2)},${round(splitY)} ${round(x2)},${round(splitY + r)}`,
    `V${round(y2)}`,
  ].join("");
}

// Keys of the item and every item above it, up to the top of the graph.
export function inputGraphPathTo(
  layout: InputGraphLayout,
  key: string | null,
): ReadonlySet<string> {
  const path = new Set<string>();
  if (key === null) return path;
  const byKey = new Map(layout.items.map((item) => [item.key, item]));
  for (let item = byKey.get(key); item && !path.has(item.key);) {
    path.add(item.key);
    item = item.parentKey === null ? undefined : byKey.get(item.parentKey);
  }
  return path;
}
