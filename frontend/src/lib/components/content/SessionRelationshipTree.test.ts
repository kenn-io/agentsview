// @vitest-environment jsdom
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";
import { mount, tick, unmount } from "svelte";
import type { Session } from "../../api/types/core.js";
import type {
  ServiceInputTree,
  ServiceInputTreeNode,
  SessionTreeResponse,
} from "../../api/generated/index.js";

const mocks = vi.hoisted(() => ({
  fetchSessionTree: vi.fn(),
  fetchSessionInputTree: vi.fn(),
}));

vi.mock("../../api/sessionTree.js", () => ({
  fetchSessionTree: mocks.fetchSessionTree,
}));

vi.mock("../../api/inputTree.js", () => ({
  fetchSessionInputTree: mocks.fetchSessionInputTree,
}));

import { m } from "../../i18n/index.js";
import { router } from "../../stores/router.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
// @ts-ignore
import SessionRelationshipTree from "./SessionRelationshipTree.svelte";

function makeSession(
  id: string,
  overrides: Partial<Session> = {},
): Session {
  return {
    compaction_count: 0,
    consecutive_failure_max: 0,
    edit_churn_count: 0,
    ended_with_role: "",
    final_failure_streak: 0,
    has_peak_context_tokens: false,
    has_total_output_tokens: false,
    mid_task_compaction_count: 0,
    outcome: "",
    outcome_confidence: "",
    secret_leak_count: 0,
    tool_failure_signal_count: 0,
    tool_retry_count: 0,
    id,
    project: "agentsview",
    machine: "test",
    agent: "codex",
    first_message: `First message ${id}`,
    started_at: "2025-01-15T10:00:00Z",
    ended_at: "2025-01-15T10:05:00Z",
    message_count: 2,
    user_message_count: 1,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    is_automated: false,
    created_at: "2025-01-15T10:00:00Z",
    ...overrides,
  };
}

function makeTree(): SessionTreeResponse {
  return {
    active_session_id: "child",
    truncated: true,
    root: {
      session: makeSession("root", {
        relationship_type: "",
        message_count: 5,
      }),
      depth: 0,
      is_active: false,
      is_leaf: false,
      is_branch_start: true,
      children: [
        {
          session: makeSession("child", {
            relationship_type: "fork",
            display_name: "Active fork",
            first_message: "Active fork preview",
          }),
          depth: 1,
          is_active: true,
          is_leaf: true,
          is_branch_start: false,
          children: [],
        },
        {
          session: makeSession("sibling", {
            relationship_type: "subagent",
            message_count: 1,
          }),
          depth: 1,
          is_active: false,
          is_leaf: true,
          is_branch_start: false,
          children: [],
        },
      ],
    },
  };
}

function inputNode(
  parent: number,
  sessionId: string,
  ordinal: number,
  preview: string,
  extra: Partial<ServiceInputTreeNode> = {},
): ServiceInputTreeNode {
  return {
    parent,
    session_id: sessionId,
    ordinal,
    preview,
    is_shell: false,
    on_active_path: false,
    end_session_ids: [],
    ...extra,
  };
}

// The fork "child" branched from "root" after their shared first input.
function makeInputTree(
  nodes: ServiceInputTreeNode[] = [
    inputNode(-1, "root", 0, "shared start", { on_active_path: true, active_ordinal: -5 }),
    inputNode(0, "root", 2, "root follow-up", { end_session_ids: ["root"] }),
    inputNode(0, "child", 0, "fork input", {
      on_active_path: true,
      active_ordinal: 0,
      end_session_ids: ["child"],
    }),
  ],
): ServiceInputTree {
  return {
    active_session_id: "child",
    root_session_id: "root",
    truncated: false,
    nodes,
    sessions: [
      makeSession("root", { relationship_type: "", message_count: 4 }),
      makeSession("child", { relationship_type: "fork", message_count: 2 }),
    ],
  };
}

async function flush() {
  await tick();
  await Promise.resolve();
  await tick();
}

// jsdom has no layout, so tests supply the sizes the component reads.
function mockRenderedHeight(tree: HTMLElement, height: number) {
  Object.defineProperty(tree, "offsetHeight", {
    configurable: true,
    value: height,
  });
}

function mockColumnHeight(column: HTMLElement, height: number) {
  Object.defineProperty(column, "clientHeight", {
    configurable: true,
    value: height,
  });
}

function getTree() {
  const tree = document.querySelector<HTMLElement>(".session-tree");
  expect(tree).not.toBeNull();
  return tree!;
}

function getResizeHandle() {
  return document.querySelector<HTMLElement>(
    `[aria-label="${m.session_tree_resize()}"]`,
  );
}

function pointerEvent(type: string, clientY: number) {
  return new PointerEvent(type, {
    bubbles: true,
    clientY,
    pointerId: 1,
  });
}

async function dragHandle(startY: number, endY: number) {
  const handle = getResizeHandle();
  expect(handle).not.toBeNull();

  handle!.dispatchEvent(pointerEvent("pointerdown", startY));
  handle!.dispatchEvent(pointerEvent("pointermove", endY));
  await tick();
  handle!.dispatchEvent(pointerEvent("pointerup", endY));
  await tick();
}

async function pressKey(key: string) {
  getResizeHandle()!.dispatchEvent(
    new KeyboardEvent("keydown", { bubbles: true, key }),
  );
  await tick();
}

describe("SessionRelationshipTree", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    mocks.fetchSessionTree.mockReset();
    mocks.fetchSessionInputTree.mockReset();
    window.history.replaceState(null, "", "/sessions/child");
    router.route = "sessions";
    router.sessionId = "child";
    router.params = {};
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    ui.setSessionTreeHeight(280);
    ui.setSessionTreeView("sessions");
    ui.activeModal = null;
    ui.clearScrollState();
    vi.restoreAllMocks();
    document.body.innerHTML = "";
  });

  it("hides single-node trees", async () => {
    mocks.fetchSessionTree.mockResolvedValue({
      active_session_id: "solo",
      truncated: false,
      root: {
        session: makeSession("solo"),
        depth: 0,
        is_active: true,
        is_leaf: true,
        is_branch_start: false,
        children: [],
      },
    });

    component = mount(SessionRelationshipTree, {
      target: document.body,
      props: { sessionId: "solo" },
    });
    await flush();

    expect(
      document.querySelector(".session-tree"),
    ).toBeNull();
    expect(getResizeHandle()).toBeNull();
  });

  it("renders active, leaf, branch, and localized labels", async () => {
    mocks.fetchSessionTree.mockResolvedValue(makeTree());

    component = mount(SessionRelationshipTree, {
      target: document.body,
      props: { sessionId: "child" },
    });
    await flush();

    expect(document.body.textContent).toContain(
      m.session_tree_title(),
    );
    expect(document.body.textContent).toContain(
      m.session_tree_truncated(),
    );
    expect(document.body.textContent).toContain(
      m.session_tree_relationship_fork(),
    );
    expect(document.body.textContent).toContain(
      m.session_tree_message_count({
        count: 1,
        countLabel: "1",
      }),
    );

    const active = document.querySelector<HTMLAnchorElement>(
      'a[href="/sessions/child"]',
    );
    expect(active).not.toBeNull();
    expect(active?.classList.contains("active")).toBe(true);
    expect(active?.classList.contains("leaf")).toBe(true);
    expect(active?.getAttribute("aria-current")).toBe("page");

    const root = document.querySelector<HTMLAnchorElement>(
      'a[href="/sessions/root"]',
    );
    expect(root?.classList.contains("branch-start")).toBe(true);
  });

  it("navigates through router when a node is clicked", async () => {
    mocks.fetchSessionTree.mockResolvedValue(makeTree());
    const navigate = vi
      .spyOn(router, "navigateToSession")
      .mockImplementation(() => {});

    component = mount(SessionRelationshipTree, {
      target: document.body,
      props: { sessionId: "child" },
    });
    await flush();

    document
      .querySelector<HTMLAnchorElement>('a[href="/sessions/sibling"]')!
      .click();

    expect(navigate).toHaveBeenCalledWith("sibling");
  });

  describe("input view", () => {
    function radio(label: string) {
      return [...document.querySelectorAll<HTMLButtonElement>('[role="radio"]')]
        .find((button) => button.textContent?.trim() === label);
    }

    function inputRow(preview: string) {
      return [...document.querySelectorAll<HTMLElement>(".input-row")]
        .find((row) => row.textContent?.includes(preview));
    }

    async function mountWithInputs(inputTree = makeInputTree()) {
      mocks.fetchSessionTree.mockResolvedValue(makeTree());
      mocks.fetchSessionInputTree.mockResolvedValue(inputTree);
      ui.setSessionTreeView("inputs");
      component = mount(SessionRelationshipTree, {
        target: document.body,
        props: { sessionId: "child" },
      });
      await flush();
      await flush();
    }

    it("is offered only when the active session belongs to a fork family", async () => {
      const tree = makeTree();
      tree.root.children[0]!.session.relationship_type = "subagent";
      mocks.fetchSessionTree.mockResolvedValue(tree);
      ui.setSessionTreeView("inputs");

      component = mount(SessionRelationshipTree, {
        target: document.body,
        props: { sessionId: "child" },
      });
      await flush();

      expect(document.querySelector('[role="radiogroup"]')).toBeNull();
      expect(radio(m.session_tree_view_inputs())).toBeUndefined();
      expect(
        document.querySelector(`[aria-label="${m.session_input_graph_open()}"]`),
      ).toBeNull();
      expect(mocks.fetchSessionInputTree).not.toHaveBeenCalled();
      expect(document.querySelector('a[href="/sessions/sibling"]')).not.toBeNull();
    });

    it("switches to the input tree and remembers the choice", async () => {
      mocks.fetchSessionTree.mockResolvedValue(makeTree());
      mocks.fetchSessionInputTree.mockResolvedValue(makeInputTree());
      component = mount(SessionRelationshipTree, {
        target: document.body,
        props: { sessionId: "child" },
      });
      await flush();

      radio(m.session_tree_view_inputs())!.click();
      await flush();
      await flush();

      expect(ui.sessionTreeView).toBe("inputs");
      expect(mocks.fetchSessionInputTree).toHaveBeenCalledWith(
        "child",
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      );
      expect(document.querySelector('a[href="/sessions/sibling"]')).toBeNull();
      expect(inputRow("shared start")?.classList.contains("branch-point")).toBe(true);
      expect(inputRow("root follow-up")?.classList.contains("leaf")).toBe(true);
      const fork = inputRow("fork input");
      expect(fork?.classList.contains("on-path")).toBe(true);
      expect(fork?.textContent).toContain(m.session_input_tree_current());
      expect(inputRow("root follow-up")?.textContent).toContain(
        m.session_tree_relationship_root(),
      );
    });

    it("opens the detailed input tree from the header", async () => {
      mocks.fetchSessionTree.mockResolvedValue(makeTree());
      component = mount(SessionRelationshipTree, {
        target: document.body,
        props: { sessionId: "child" },
      });
      await flush();

      const open = document.querySelector<HTMLButtonElement>(
        `[aria-label="${m.session_input_graph_open()}"]`,
      );
      expect(open).not.toBeNull();
      open!.click();

      expect(ui.activeModal).toBe("inputTree");
    });

    it("jumps within the open session to an input on its path", async () => {
      const navigate = vi
        .spyOn(router, "navigateToSession")
        .mockImplementation(() => {});
      await mountWithInputs();

      const shared = inputRow("shared start") as HTMLAnchorElement;
      expect(shared.getAttribute("href")).toBe("/sessions/child?msg=-5");
      shared.click();

      expect(ui.selectedOrdinal).toBe(-5);
      expect(ui.pendingScrollOrdinal).toBe(-5);
      expect(ui.pendingScrollSession).toBeNull();
      expect(navigate).not.toHaveBeenCalled();
    });

    it("opens the session that holds an input off the active path", async () => {
      const navigate = vi
        .spyOn(router, "navigateToSession")
        .mockImplementation(() => {});
      await mountWithInputs();

      const other = inputRow("root follow-up") as HTMLAnchorElement;
      expect(other.getAttribute("href")).toBe("/sessions/root?msg=2");
      other.click();

      expect(navigate).toHaveBeenCalledWith("root");
      expect(ui.pendingScrollOrdinal).toBe(2);
      expect(ui.pendingScrollSession).toBe("root");
    });

    // root ─ child (fork) ─ moved (continuation of child)
    function makeContinuedTree(activeId: string): SessionTreeResponse {
      const moved = {
        session: makeSession("moved", { relationship_type: "continuation" }),
        depth: 2,
        is_active: activeId === "moved",
        is_leaf: true,
        is_branch_start: false,
        children: [],
      };
      return {
        active_session_id: activeId,
        truncated: false,
        root: {
          session: makeSession("root", { relationship_type: "" }),
          depth: 0,
          is_active: activeId === "root",
          is_leaf: false,
          is_branch_start: false,
          children: [
            {
              session: makeSession("child", { relationship_type: "fork" }),
              depth: 1,
              is_active: activeId === "child",
              is_leaf: false,
              is_branch_start: false,
              children: [moved],
            },
          ],
        },
      };
    }

    // The inputs of makeContinuedTree, opened from moved: its path holds
    // root's first input and child's input, which its message view does
    // not show.
    function makeContinuedInputTree(): ServiceInputTree {
      return {
        active_session_id: "moved",
        root_session_id: "root",
        truncated: false,
        nodes: [
          inputNode(-1, "root", 0, "shared start", { on_active_path: true }),
          inputNode(0, "root", 2, "root follow-up", { end_session_ids: ["root"] }),
          inputNode(0, "child", 0, "fork input", {
            on_active_path: true,
            end_session_ids: ["child"],
          }),
          inputNode(2, "moved", 2, "moved input", {
            on_active_path: true,
            active_ordinal: 2,
            end_session_ids: ["moved"],
          }),
        ],
        sessions: [
          makeSession("root", { relationship_type: "" }),
          makeSession("child", { relationship_type: "fork" }),
          makeSession("moved", { relationship_type: "continuation" }),
        ],
      };
    }

    async function mountContinued(activeId: string, inputTree: ServiceInputTree) {
      mocks.fetchSessionTree.mockResolvedValue(makeContinuedTree(activeId));
      mocks.fetchSessionInputTree.mockResolvedValue(inputTree);
      ui.setSessionTreeView("inputs");
      component = mount(SessionRelationshipTree, {
        target: document.body,
        props: { sessionId: activeId },
      });
      await flush();
      await flush();
    }

    it("is offered for a continuation of a fork", async () => {
      await mountContinued("moved", makeContinuedInputTree());

      expect(radio(m.session_tree_view_inputs())).not.toBeUndefined();
      expect(mocks.fetchSessionInputTree).toHaveBeenCalledWith(
        "moved",
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      );
      expect(inputRow("moved input")?.textContent).toContain(m.session_input_tree_current());
    });

    it("is offered for a family linked by continuations alone", async () => {
      // root ─ moved (continuation)
      const tree = makeContinuedTree("moved");
      tree.root.children = tree.root.children[0]!.children;
      mocks.fetchSessionTree.mockResolvedValue(tree);
      mocks.fetchSessionInputTree.mockResolvedValue({
        active_session_id: "moved",
        root_session_id: "root",
        truncated: false,
        nodes: [
          inputNode(-1, "root", 0, "shared start", { on_active_path: true }),
          inputNode(0, "root", 2, "root end", {
            on_active_path: true,
            end_session_ids: ["root"],
          }),
          inputNode(1, "moved", 0, "moved input", {
            on_active_path: true,
            active_ordinal: 0,
            end_session_ids: ["moved"],
          }),
        ],
        sessions: [
          makeSession("root", { relationship_type: "" }),
          makeSession("moved", { relationship_type: "continuation" }),
        ],
      } satisfies ServiceInputTree);
      ui.setSessionTreeView("inputs");
      component = mount(SessionRelationshipTree, {
        target: document.body,
        props: { sessionId: "moved" },
      });
      await flush();
      await flush();

      expect(radio(m.session_tree_view_inputs())).not.toBeUndefined();
      expect(
        document.querySelector(`[aria-label="${m.session_input_graph_open()}"]`),
      ).not.toBeNull();
      expect(mocks.fetchSessionInputTree).toHaveBeenCalledWith(
        "moved",
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      );
      expect(inputRow("root end")?.textContent).toContain(
        m.session_tree_relationship_root(),
      );
      expect(inputRow("moved input")?.textContent).toContain(
        m.session_input_tree_current(),
      );
    });

    it("opens the session that holds an input a continuation carries on", async () => {
      const navigate = vi
        .spyOn(router, "navigateToSession")
        .mockImplementation(() => {});
      await mountContinued("moved", makeContinuedInputTree());

      const carried = inputRow("fork input") as HTMLAnchorElement;
      expect(carried.classList.contains("on-path")).toBe(true);
      expect(carried.textContent).toContain(m.session_tree_relationship_fork());
      expect(carried.getAttribute("href")).toBe("/sessions/child?msg=0");
      carried.click();

      expect(navigate).toHaveBeenCalledWith("child");
      expect(ui.pendingScrollOrdinal).toBe(0);
      expect(ui.pendingScrollSession).toBe("child");
    });

    it("labels the end of a continuation below the open session", async () => {
      const inputTree = makeContinuedInputTree();
      inputTree.active_session_id = "child";
      inputTree.nodes = [
        inputNode(-1, "root", 0, "shared start", {
          on_active_path: true,
          active_ordinal: -5,
        }),
        inputNode(0, "root", 2, "root follow-up", { end_session_ids: ["root"] }),
        inputNode(0, "child", 0, "fork input", {
          on_active_path: true,
          active_ordinal: 0,
          end_session_ids: ["child"],
        }),
        inputNode(2, "moved", 2, "moved input", { end_session_ids: ["moved"] }),
      ];
      await mountContinued("child", inputTree);

      const moved = inputRow("moved input");
      expect(moved?.classList.contains("on-path")).toBe(false);
      expect(moved?.textContent).toContain(m.session_tree_relationship_continuation());
      expect(inputRow("fork input")?.textContent).toContain(m.session_input_tree_current());
    });

    it("folds long runs of inputs until opened", async () => {
      await mountWithInputs(
        makeInputTree([
          inputNode(-1, "root", 0, "input 0", { on_active_path: true, active_ordinal: 0 }),
          inputNode(0, "root", 2, "input 1", { on_active_path: true, active_ordinal: 2 }),
          inputNode(1, "root", 4, "input 2", { on_active_path: true, active_ordinal: 4 }),
          inputNode(2, "root", 6, "input 3", { on_active_path: true, active_ordinal: 6 }),
          inputNode(3, "root", 8, "input 4", { on_active_path: true, active_ordinal: 8 }),
          inputNode(4, "root", 10, "root end", { end_session_ids: ["root"] }),
          inputNode(4, "child", 0, "fork end", {
            on_active_path: true,
            active_ordinal: 0,
            end_session_ids: ["child"],
          }),
        ]),
      );

      const gap = document.querySelector<HTMLButtonElement>(".input-row.gap");
      expect(gap?.textContent).toContain(
        m.session_input_tree_hidden({ count: 3, countLabel: "3" }),
      );
      expect(inputRow("input 2")).toBeUndefined();

      gap!.click();
      await flush();

      expect(document.querySelector(".input-row.gap")).toBeNull();
      expect(inputRow("input 2")).not.toBeUndefined();
    });

    it("keeps the selected input visible inside a folded run", async () => {
      ui.selectOrdinal(4);
      await mountWithInputs(
        makeInputTree([
          inputNode(-1, "root", 0, "input 0", { on_active_path: true, active_ordinal: 0 }),
          inputNode(0, "root", 2, "input 1", { on_active_path: true, active_ordinal: 2 }),
          inputNode(1, "root", 4, "input 2", { on_active_path: true, active_ordinal: 4 }),
          inputNode(2, "root", 6, "input 3", { on_active_path: true, active_ordinal: 6 }),
          inputNode(3, "root", 8, "input 4", { on_active_path: true, active_ordinal: 8 }),
          inputNode(4, "root", 10, "root end", { end_session_ids: ["root"] }),
          inputNode(4, "child", 0, "fork end", {
            on_active_path: true,
            active_ordinal: 0,
            end_session_ids: ["child"],
          }),
        ]),
      );

      expect(document.querySelector(".input-row.gap")).toBeNull();
      expect(inputRow("input 2")?.classList.contains("selected")).toBe(true);
    });

    it("marks a partial input tree", async () => {
      await mountWithInputs({ ...makeInputTree(), truncated: true });

      expect(document.querySelector(".input-state.truncated")?.textContent).toBe(
        m.session_tree_truncated(),
      );
    });

    it("reports a failed load", async () => {
      vi.spyOn(console, "error").mockImplementation(() => {});
      mocks.fetchSessionTree.mockResolvedValue(makeTree());
      mocks.fetchSessionInputTree.mockRejectedValue(new Error("boom"));
      ui.setSessionTreeView("inputs");
      component = mount(SessionRelationshipTree, {
        target: document.body,
        props: { sessionId: "child" },
      });
      await flush();
      await flush();

      expect(document.body.textContent).toContain(m.session_input_tree_error());
    });
  });

  describe("height", () => {
    // Stands in for the vitals column the tree renders into.
    let column: HTMLElement;

    beforeEach(() => {
      mocks.fetchSessionTree.mockResolvedValue(makeTree());
      column = document.createElement("div");
      document.body.appendChild(column);
    });

    async function mountInColumn() {
      component = mount(SessionRelationshipTree, {
        target: column,
        props: { sessionId: "child" },
      });
      await flush();
    }

    it("limits the tree to the stored height", async () => {
      ui.setSessionTreeHeight(360);

      await mountInColumn();

      expect(getTree().style.maxHeight).toBe("360px");
      expect(getResizeHandle()?.getAttribute("aria-valuenow")).toBe(
        "360",
      );
    });

    it("grows a short tree from its rendered height when dragged down", async () => {
      await mountInColumn();
      mockRenderedHeight(getTree(), 150);

      await dragHandle(400, 460);

      expect(ui.sessionTreeHeight).toBe(210);
      expect(getTree().style.maxHeight).toBe("210px");
    });

    it("shrinks from arrow keys and stops at the minimum", async () => {
      await mountInColumn();
      mockRenderedHeight(getTree(), 280);

      await pressKey("ArrowUp");
      expect(ui.sessionTreeHeight).toBe(256);

      await dragHandle(400, 0);
      expect(ui.sessionTreeHeight).toBe(96);
      expect(getTree().style.maxHeight).toBe("96px");
    });

    it("keeps room for the session vitals below the tree", async () => {
      mockColumnHeight(column, 500);
      await mountInColumn();
      mockRenderedHeight(getTree(), 280);

      await dragHandle(400, 700);

      // A 500px column keeps 160px for the handle and vitals.
      expect(ui.sessionTreeHeight).toBe(340);
      expect(getTree().style.maxHeight).toBe("340px");
      expect(getResizeHandle()?.getAttribute("aria-valuemax")).toBe(
        "340",
      );
    });

    it("fits a taller stored height to a short column without forgetting it", async () => {
      ui.setSessionTreeHeight(600);
      mockColumnHeight(column, 500);

      await mountInColumn();
      expect(getTree().style.maxHeight).toBe("340px");

      mockRenderedHeight(getTree(), 340);
      await dragHandle(400, 480);

      expect(getTree().style.maxHeight).toBe("340px");
      expect(ui.sessionTreeHeight).toBe(600);
    });
  });

  describe("active node scrolling", () => {
    const ROW_HEIGHT = 50;
    const LIST_HEIGHT = 200;

    // A root with `count - 1` subagent children; the child at
    // `activeIndex` (1-based row) is the active session.
    function makeWideTree(count: number, activeIndex: number) {
      const children = Array.from({ length: count - 1 }, (_, i) => ({
        session: makeSession(`node-${i + 1}`, {
          relationship_type: "subagent",
        }),
        depth: 1,
        is_active: i + 1 === activeIndex,
        is_leaf: true,
        is_branch_start: false,
        children: [],
      }));
      return {
        active_session_id: `node-${activeIndex}`,
        truncated: false,
        root: {
          session: makeSession("root"),
          depth: 0,
          is_active: false,
          is_leaf: false,
          is_branch_start: true,
          children,
        },
      } satisfies SessionTreeResponse;
    }

    // Lays rows out top to bottom at ROW_HEIGHT each inside a list that
    // shows LIST_HEIGHT pixels.
    beforeEach(() => {
      vi.spyOn(HTMLElement.prototype, "offsetTop", "get").mockImplementation(
        function (this: HTMLElement) {
          if (!this.classList.contains("tree-node")) return 0;
          const rows = Array.from(this.parentElement?.children ?? []);
          return rows.indexOf(this) * ROW_HEIGHT;
        },
      );
      vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockImplementation(
        function (this: HTMLElement) {
          return this.classList.contains("tree-node") ? ROW_HEIGHT : 0;
        },
      );
      vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockImplementation(
        function (this: HTMLElement) {
          return this.classList.contains("tree-list") ? LIST_HEIGHT : 0;
        },
      );
    });

    function getList() {
      const list = document.querySelector<HTMLElement>(".tree-list");
      expect(list).not.toBeNull();
      return list!;
    }

    it("centers an active node that loads below the visible rows", async () => {
      mocks.fetchSessionTree.mockResolvedValue(makeWideTree(12, 9));

      component = mount(SessionRelationshipTree, {
        target: document.body,
        props: { sessionId: "node-9" },
      });
      await flush();
      await tick();

      // Row 9 spans 450-500px; centering it in a 200px list puts the top
      // of the view at 450 - (200 - 50) / 2.
      expect(getList().scrollTop).toBe(375);
    });

    it("leaves the list at the top when the active node is already visible", async () => {
      mocks.fetchSessionTree.mockResolvedValue(makeWideTree(12, 2));

      component = mount(SessionRelationshipTree, {
        target: document.body,
        props: { sessionId: "node-2" },
      });
      await flush();
      await tick();

      expect(getList().scrollTop).toBe(0);
    });
  });
});
