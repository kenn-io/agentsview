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
import { cleanup, render } from "@testing-library/svelte";
import type { Session } from "../../api/types/core.js";
import type { SessionTiming } from "../../api/types/timing.js";

const mocks = vi.hoisted(() => {
  const timing: SessionTiming = {
    session_id: "sess-1",
    total_duration_ms: 1200,
    tool_duration_ms: 0,
    turn_count: 1,
    tool_call_count: 0,
    subagent_count: 0,
    slowest_call: null,
    by_category: [],
    turns: [],
    running: false,
  };

  return {
    fetchSessionTiming: vi.fn().mockResolvedValue(timing),
    timing,
  };
});

const traceSession: Session = {
  id: "sess-1",
  project: "agentsview",
  machine: "local",
  agent: "codex",
  first_message: "hello",
  started_at: "2026-07-14T12:00:00Z",
  ended_at: "2026-07-14T12:01:00Z",
  message_count: 2,
  user_message_count: 1,
  total_output_tokens: 0,
  peak_context_tokens: 0,
  is_automated: false,
  created_at: "2026-07-14T12:00:00Z",
  cwd: "/repos/agentsview/.worktrees/trace-context",
};

vi.mock("../../api/timing.js", () => ({
  fetchSessionTiming: mocks.fetchSessionTiming,
}));

import { ui } from "../../stores/ui.svelte.js";
import { sessionTiming } from "../../stores/sessionTiming.svelte.js";
import { m } from "../../i18n/index.js";
// @ts-ignore
import SessionVitals from "./SessionVitals.svelte";

describe("SessionVitals", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    mocks.fetchSessionTiming.mockReset().mockResolvedValue(mocks.timing);
    sessionTiming.reset();
    ui.vitalsOpen = true;
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    sessionTiming.reset();
    ui.vitalsOpen = false;
    cleanup();
    document.body.innerHTML = "";
    vi.unstubAllGlobals();
  });

  it("has an obvious close control inside the analysis pane", async () => {
    component = mount(SessionVitals, {
      target: document.body,
      props: { sessionId: "sess-1", session: undefined },
    });
    await tick();
    await tick();

    const closeButton = document.querySelector<HTMLButtonElement>(
      `button[aria-label="${m.session_vitals_close()}"]`,
    );

    expect(closeButton).not.toBeNull();
    expect(closeButton?.title).toBe(m.session_vitals_close());

    closeButton!.click();
    await tick();

    expect(ui.vitalsOpen).toBe(false);
  });

  it("shows the repository and worktree recorded by the trace", async () => {
    component = mount(SessionVitals, {
      target: document.body,
      props: { sessionId: traceSession.id, session: traceSession },
    });
    await tick();
    await tick();

    const rows = document.querySelectorAll(".context-row");
    expect(rows).toHaveLength(2);
    expect(rows[0]?.querySelector(".context-label")?.textContent?.trim()).toBe(
      m.session_vitals_repository(),
    );
    expect(rows[0]?.querySelector(".context-value")?.textContent?.trim()).toBe(
      traceSession.project,
    );
    expect(rows[1]?.querySelector(".context-label")?.textContent?.trim()).toBe(
      m.session_vitals_worktree(),
    );
    expect(rows[1]?.querySelector(".context-value")?.textContent?.trim()).toBe(
      traceSession.cwd,
    );
    expect(
      document.querySelector('[title="agentsview"]'),
    ).not.toBeNull();
    expect(
      document.querySelector(
        '[title="/repos/agentsview/.worktrees/trace-context"]',
      ),
    ).not.toBeNull();
  });

  it("keeps trace context visible when timing fails to load", async () => {
    mocks.fetchSessionTiming.mockRejectedValueOnce(
      new Error("timing unavailable"),
    );
    component = mount(SessionVitals, {
      target: document.body,
      props: { sessionId: traceSession.id, session: traceSession },
    });
    await tick();
    await Promise.resolve();
    await tick();

    expect(document.querySelector(".session-context")).not.toBeNull();
    expect(document.body.textContent).toContain(traceSession.project);
    expect(document.body.textContent).toContain(traceSession.cwd);
    expect(document.body.textContent).toContain("timing unavailable");
  });

  it("copies repository and worktree values from hover controls", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    component = mount(SessionVitals, {
      target: document.body,
      props: { sessionId: traceSession.id, session: traceSession },
    });
    await tick();
    await tick();

    const repositoryCopy = document.querySelector<HTMLButtonElement>(
      `button[aria-label="${m.session_vitals_copy_repository()}"]`,
    );
    const worktreeCopy = document.querySelector<HTMLButtonElement>(
      `button[aria-label="${m.session_vitals_copy_worktree()}"]`,
    );
    expect(repositoryCopy).not.toBeNull();
    expect(worktreeCopy).not.toBeNull();
    expect(repositoryCopy?.classList).toContain("kit-copy-btn--reveal");
    expect(worktreeCopy?.classList).toContain("kit-copy-btn--reveal");

    repositoryCopy!.click();
    await Promise.resolve();
    expect(writeText).toHaveBeenNthCalledWith(1, traceSession.project);

    worktreeCopy!.click();
    await Promise.resolve();
    expect(writeText).toHaveBeenNthCalledWith(2, traceSession.cwd);
  });

  it("aborts a pending sub-agent timing read when collapsed", async () => {
    const signals: AbortSignal[] = [];
    mocks.fetchSessionTiming.mockImplementation(
      (sessionId: string, signal?: AbortSignal) => {
        if (sessionId === "sess-1") return Promise.resolve(mocks.timing);
        if (signal) signals.push(signal);
        return new Promise<SessionTiming>(() => {});
      },
    );
    component = mount(SessionVitals, {
      target: document.body,
      props: { sessionId: "sess-1", session: undefined },
    });
    await tick();
    await Promise.resolve();
    await tick();
    sessionTiming.applyEvent(parentTimingWithSubagent());
    await tick();

    const toggle = document.querySelector<HTMLButtonElement>(
      `button[aria-label="${m.call_row_toggle_subagent_calls()}"]`,
    );
    expect(toggle).not.toBeNull();
    toggle!.click();
    await tick();
    expect(signals).toHaveLength(1);

    toggle!.click();
    await tick();

    expect(signals[0]?.aborted).toBe(true);
  });

  it("aborts a pending sub-agent timing read when unmounted", async () => {
    const signals: AbortSignal[] = [];
    mocks.fetchSessionTiming.mockImplementation(
      (sessionId: string, signal?: AbortSignal) => {
        if (sessionId === "sess-1") return Promise.resolve(mocks.timing);
        if (signal) signals.push(signal);
        return new Promise<SessionTiming>(() => {});
      },
    );
    component = mount(SessionVitals, {
      target: document.body,
      props: { sessionId: "sess-1", session: undefined },
    });
    await tick();
    await Promise.resolve();
    await tick();
    sessionTiming.applyEvent(parentTimingWithSubagent());
    await tick();

    document
      .querySelector<HTMLButtonElement>(
        `button[aria-label="${m.call_row_toggle_subagent_calls()}"]`,
      )!
      .click();
    await tick();
    expect(signals).toHaveLength(1);

    unmount(component);
    component = undefined;

    expect(signals[0]?.aborted).toBe(true);
  });

  it("aborts a pending sub-agent timing read when the parent changes", async () => {
    const signals: AbortSignal[] = [];
    mocks.fetchSessionTiming.mockImplementation(
      (sessionId: string, signal?: AbortSignal) => {
        if (sessionId.startsWith("sess-")) {
          return Promise.resolve(mocks.timing);
        }
        if (signal) signals.push(signal);
        return new Promise<SessionTiming>(() => {});
      },
    );
    const view = render(SessionVitals, {
      sessionId: "sess-1",
      session: undefined,
    });
    await tick();
    await Promise.resolve();
    await tick();
    sessionTiming.applyEvent(parentTimingWithSubagent());
    await tick();

    document
      .querySelector<HTMLButtonElement>(
        `button[aria-label="${m.call_row_toggle_subagent_calls()}"]`,
      )!
      .click();
    await tick();
    expect(signals).toHaveLength(1);

    await view.rerender({ sessionId: "sess-2" });
    await tick();

    expect(signals[0]?.aborted).toBe(true);
  });
});

function parentTimingWithSubagent(): SessionTiming {
  return {
    ...mocks.timing,
    tool_duration_ms: 400,
    tool_call_count: 1,
    subagent_count: 1,
    turns: [
      {
        message_id: 1,
        ordinal: 1,
        started_at: "2026-07-14T12:00:00Z",
        duration_ms: 400,
        primary_category: "task",
        calls: [
          {
            tool_use_id: "call-1",
            tool_name: "Task",
            category: "task",
            subagent_session_id: "child-1",
            duration_ms: 400,
            is_parallel: false,
            input_preview: "delegate",
          },
        ],
      },
    ],
  };
}
