// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { Session } from "../../api/types/core.js";
import type { ServiceInputTree, ServiceInputTreeNode } from "../../api/generated/index.js";

const mocks = vi.hoisted(() => ({
  fetchSessionInputTree: vi.fn(),
}));

vi.mock("../../api/inputTree.js", () => ({
  fetchSessionInputTree: mocks.fetchSessionInputTree,
}));

import { m } from "../../i18n/index.js";
import { router } from "../../stores/router.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
// @ts-ignore
import InputTreeModal from "./InputTreeModal.svelte";

function makeSession(id: string, overrides: Partial<Session> = {}): Session {
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
    agent: "claude",
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
      makeSession("root", {
        relationship_type: "",
        display_name: "Root session",
        message_count: 4,
      }),
      makeSession("child", {
        relationship_type: "fork",
        display_name: "Fork session",
        message_count: 2,
      }),
    ],
  };
}

// 0 ─ 1 ─ 2 ─ 3 ─ 4 ─┬ "root end"
//                    └ "fork end"
function runTree(): ServiceInputTree {
  return makeInputTree([
    inputNode(-1, "root", 0, "input 0", { on_active_path: true, active_ordinal: -10 }),
    inputNode(0, "root", 2, "input 1", { on_active_path: true, active_ordinal: -8 }),
    inputNode(1, "root", 4, "input 2", { on_active_path: true, active_ordinal: -6 }),
    inputNode(2, "root", 6, "input 3", { on_active_path: true, active_ordinal: -4 }),
    inputNode(3, "root", 8, "input 4", { on_active_path: true, active_ordinal: -2 }),
    inputNode(4, "root", 10, "root end", { end_session_ids: ["root"] }),
    inputNode(4, "child", 0, "fork end", {
      on_active_path: true,
      active_ordinal: 0,
      end_session_ids: ["child"],
    }),
  ]);
}

async function flush() {
  await tick();
  await Promise.resolve();
  await tick();
}

function inputCard(preview: string) {
  return [...document.querySelectorAll<HTMLAnchorElement>(".input-node")].find((card) =>
    card.textContent?.includes(preview),
  );
}

function sessionCard(id: string) {
  return document.querySelector<HTMLAnchorElement>(`.session-node[href="/sessions/${id}"]`);
}

function byLabel<T extends HTMLElement>(label: string) {
  return document.querySelector<T>(`[aria-label="${label}"]`);
}

describe("InputTreeModal", () => {
  let component: ReturnType<typeof mount> | undefined;
  let navigate: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    mocks.fetchSessionInputTree.mockReset();
    window.history.replaceState(null, "", "/sessions/child");
    router.route = "sessions";
    router.sessionId = "child";
    router.params = {};
    navigate = vi.spyOn(router, "navigateToSession").mockImplementation(() => {});
    ui.activeModal = "inputTree";
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    ui.activeModal = null;
    ui.clearScrollState();
    vi.restoreAllMocks();
    document.body.innerHTML = "";
  });

  async function open(inputTree: ServiceInputTree = makeInputTree(), sessionId = "child") {
    mocks.fetchSessionInputTree.mockResolvedValue(inputTree);
    component = mount(InputTreeModal, {
      target: document.body,
      props: { sessionId },
    });
    await flush();
    await flush();
  }

  it("draws inputs as cards with a session card at the end of every branch", async () => {
    await open();

    expect(mocks.fetchSessionInputTree).toHaveBeenCalledWith(
      "child",
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(document.body.textContent).toContain(
      m.session_input_graph_sessions({ count: 2, countLabel: "2" }),
    );
    expect(document.body.textContent).toContain(
      m.session_input_graph_inputs({ count: 3, countLabel: "3" }),
    );

    expect(inputCard("shared start")?.textContent).toContain(
      m.session_input_graph_branches({ count: 2, countLabel: "2" }),
    );
    expect(inputCard("fork input")?.classList.contains("on-path")).toBe(true);
    expect(inputCard("root follow-up")?.classList.contains("on-path")).toBe(false);

    const current = sessionCard("child");
    expect(current?.classList.contains("current")).toBe(true);
    expect(current?.getAttribute("aria-current")).toBe("page");
    expect(current?.textContent).toContain("Fork session");
    expect(current?.textContent).toContain(m.session_input_tree_current());
    const other = sessionCard("root");
    expect(other?.classList.contains("current")).toBe(false);
    expect(other?.textContent).toContain("Root session");
    expect(other?.textContent).toContain(m.session_tree_relationship_root());
    expect(other?.textContent).toContain(
      m.session_tree_message_count({ count: 4, countLabel: "4" }),
    );

    // The open session's path: shared start -> fork input -> its session.
    expect(document.querySelectorAll(".graph-edges path.on-path")).toHaveLength(2);
    expect(document.querySelectorAll(".graph-edges path")).toHaveLength(4);
  });

  it("jumps within the open session to an input on its path and closes", async () => {
    await open();

    const shared = inputCard("shared start")!;
    expect(shared.getAttribute("href")).toBe("/sessions/child?msg=-5");
    shared.click();

    expect(ui.selectedOrdinal).toBe(-5);
    expect(ui.pendingScrollOrdinal).toBe(-5);
    expect(ui.pendingScrollSession).toBeNull();
    expect(navigate).not.toHaveBeenCalled();
    expect(ui.activeModal).toBeNull();
  });

  it("opens the session that holds an input off the active path", async () => {
    await open();

    const other = inputCard("root follow-up")!;
    expect(other.getAttribute("href")).toBe("/sessions/root?msg=2");
    other.click();

    expect(navigate).toHaveBeenCalledWith("root");
    expect(ui.pendingScrollOrdinal).toBe(2);
    expect(ui.pendingScrollSession).toBe("root");
    expect(ui.activeModal).toBeNull();
  });

  it("keeps the inputs a continuation carries on on its path and opens their session", async () => {
    // moved continues child, so its path holds child's input, which its
    // message view does not show.
    const inputTree = makeInputTree([
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
    ]);
    inputTree.active_session_id = "moved";
    inputTree.sessions.push(makeSession("moved", { relationship_type: "continuation" }));
    await open(inputTree, "moved");

    expect(inputCard("shared start")?.classList.contains("on-path")).toBe(true);
    expect(inputCard("moved input")?.getAttribute("href")).toBe("/sessions/moved?msg=2");
    const carried = inputCard("fork input")!;
    expect(carried.classList.contains("on-path")).toBe(true);
    expect(carried.getAttribute("href")).toBe("/sessions/child?msg=0");
    carried.click();

    expect(navigate).toHaveBeenCalledWith("child");
    expect(ui.pendingScrollOrdinal).toBe(0);
    expect(ui.pendingScrollSession).toBe("child");
    expect(ui.activeModal).toBeNull();
  });

  it("leaves modified clicks to the browser", async () => {
    await open();
    // Record whether the card claimed the click, then stop jsdom from
    // following the link.
    let claimed: boolean | undefined;
    document.addEventListener(
      "click",
      (event) => {
        claimed = event.defaultPrevented;
        event.preventDefault();
      },
      { once: true },
    );

    inputCard("root follow-up")!.dispatchEvent(
      new MouseEvent("click", { bubbles: true, cancelable: true, ctrlKey: true }),
    );

    expect(claimed).toBe(false);
    expect(navigate).not.toHaveBeenCalled();
    expect(ui.activeModal).toBe("inputTree");
  });

  it("opens a session from its card", async () => {
    await open();

    sessionCard("root")!.click();
    expect(navigate).toHaveBeenCalledWith("root");
    expect(ui.activeModal).toBeNull();
  });

  it("closes without navigating when the open session's card is clicked", async () => {
    await open();

    sessionCard("child")!.click();
    expect(navigate).not.toHaveBeenCalled();
    expect(ui.activeModal).toBeNull();
  });

  it("highlights the path above a hovered card", async () => {
    await open();

    sessionCard("root")!.dispatchEvent(new MouseEvent("mouseenter"));
    await tick();

    expect(inputCard("root follow-up")?.classList.contains("hovered")).toBe(true);
    expect(inputCard("shared start")?.classList.contains("hovered")).toBe(true);
    expect(inputCard("fork input")?.classList.contains("hovered")).toBe(false);
    expect(document.querySelectorAll(".graph-edges path.hover-path")).toHaveLength(2);

    sessionCard("root")!.dispatchEvent(new MouseEvent("mouseleave"));
    await tick();
    expect(document.querySelectorAll(".graph-edges path.hover-path")).toHaveLength(0);
  });

  it("folds runs of inputs until opened or folding is turned off", async () => {
    await open(runTree());

    const run = document.querySelector<HTMLButtonElement>(".run-node");
    expect(run?.textContent).toContain(m.session_input_tree_hidden({ count: 3, countLabel: "3" }));
    expect(inputCard("input 2")).toBeUndefined();

    run!.click();
    await flush();
    expect(document.querySelector(".run-node")).toBeNull();
    expect(inputCard("input 2")).toBeDefined();

    // Turning folding off and on again forgets the opened run.
    const fold = document.querySelector<HTMLInputElement>('input[role="switch"]')!;
    expect(fold.checked).toBe(true);
    fold.click();
    await flush();
    expect(document.querySelector(".run-node")).toBeNull();
    fold.click();
    await flush();
    expect(document.querySelector(".run-node")).not.toBeNull();
  });

  it("keeps the selected input visible inside a folded run", async () => {
    ui.selectOrdinal(-6);
    await open(runTree());

    expect(document.querySelector(".run-node")).toBeNull();
    const selected = inputCard("input 2");
    expect(selected?.classList.contains("selected")).toBe(true);
    expect(selected?.getAttribute("aria-current")).toBe("step");
  });

  it("zooms in steps and resets to 100%", async () => {
    await open();
    const reset = byLabel<HTMLButtonElement>(m.session_input_graph_zoom_reset())!;
    const sizer = document.querySelector<HTMLElement>(".graph-sizer")!;
    const stage = document.querySelector<HTMLElement>(".graph-stage")!;
    expect(reset.textContent?.trim()).toBe("100%");
    expect(sizer.style.width).toBe("576px");

    byLabel<HTMLButtonElement>(m.session_input_graph_zoom_in())!.click();
    await flush();
    expect(reset.textContent?.trim()).toBe("125%");
    expect(sizer.style.width).toBe("720px");
    expect(stage.style.transform).toBe("scale(1.25)");

    reset.click();
    await flush();
    expect(reset.textContent?.trim()).toBe("100%");

    const zoomOut = byLabel<HTMLButtonElement>(m.session_input_graph_zoom_out())!;
    for (let i = 0; i < 6; i++) {
      zoomOut.click();
      await flush();
    }
    expect(reset.textContent?.trim()).toBe("25%");
    expect(zoomOut.disabled).toBe(true);
  });

  it("marks a partial tree", async () => {
    await open({ ...makeInputTree(), truncated: true });

    expect(document.querySelector(".graph-truncated")?.textContent).toBe(
      m.session_tree_truncated(),
    );
  });

  it("reports a failed load", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    mocks.fetchSessionInputTree.mockRejectedValue(new Error("boom"));
    component = mount(InputTreeModal, {
      target: document.body,
      props: { sessionId: "child" },
    });
    await flush();
    await flush();

    expect(document.querySelector(".graph-state.error")?.textContent).toBe(
      m.session_input_tree_error(),
    );
    expect(document.querySelector(".graph-viewport")).toBeNull();
  });
});
