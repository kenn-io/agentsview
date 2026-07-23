// @vitest-environment jsdom
import {
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "./lib/stores/analytics.svelte.js";
import { analyticsPageDates } from "./lib/stores/analyticsPageDates.js";
import { insights } from "./lib/stores/insights.svelte.js";
import { messages } from "./lib/stores/messages.svelte.js";
import { pins } from "./lib/stores/pins.svelte.js";
import { router } from "./lib/stores/router.svelte.js";
import { rollingRange } from "./lib/utils/dates.js";
import { sessionTiming } from "./lib/stores/sessionTiming.svelte.js";
import { sessions } from "./lib/stores/sessions.svelte.js";
import { settings } from "./lib/stores/settings.svelte.js";
import { starred } from "./lib/stores/starred.svelte.js";
import { sync } from "./lib/stores/sync.svelte.js";
import { ui } from "./lib/stores/ui.svelte.js";
import { usage } from "./lib/stores/usage.svelte.js";
import { yokedDates } from "./lib/stores/yokedDates.svelte.js";
import type { Message } from "./lib/api/types.js";
import { hasVisibleSegments } from "./lib/utils/content-parser.js";
import sourceRaw from "./App.svelte?raw";
import { SESSION_FILTER_KEYS } from "./lib/stores/sessionRouteParams.js";
// @ts-ignore
import App, { findUserPromptOrdinal } from "./App.svelte";

const source = sourceRaw.replace(/\r\n/g, "\n");

let component: ReturnType<typeof mount> | undefined;

async function flushEffects() {
  await tick();
  await Promise.resolve();
  await tick();
}

async function selectRelativeRange(days: number) {
  const trigger = document.querySelector<HTMLButtonElement>(
    ".kit-date-range-picker__trigger",
  );
  expect(trigger).not.toBeNull();
  trigger!.click();
  await flushEffects();

  const preset = [
    ...document.querySelectorAll<HTMLButtonElement>("button"),
  ].find((button) => button.textContent?.trim() === `${days}d`);
  expect(preset).not.toBeUndefined();
  preset!.click();
  await flushEffects();
}

afterEach(() => {
  if (component) {
    unmount(component);
    component = undefined;
  }
  vi.restoreAllMocks();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  document.body.innerHTML = "";
  localStorage.clear();
  window.history.replaceState(null, "", "/");
  router.route = "sessions";
  router.params = {};
  router.sessionId = null;
  sessions.activeSessionId = null;
  sessions.filters.date = "";
  sessions.filters.dateFrom = "";
  sessions.filters.dateTo = "";
  sessions.filters.project = "";
  analytics.applyRollingWindow(365);
  usage.isPinned = false;
  usage.windowDays = 30;
  usage.from = "";
  usage.to = "";
  analyticsPageDates.clear();
  yokedDates.setEnabled(false);
  ui.clearScrollState();
  settings.needsAuth = false;
});

function appSourceSlice(startMarker: string, endMarker: string): string {
  const start = source.indexOf(startMarker);
  expect(start).toBeGreaterThan(-1);
  const end = source.indexOf(endMarker, start);
  expect(end).toBeGreaterThan(start);
  return source.slice(start, end);
}

describe("App session URL date state", () => {
  it("cancels session reads on route exit and restarts an incomplete load on return", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(settings, "load").mockResolvedValue();
    vi.spyOn(starred, "load").mockResolvedValue();
    vi.spyOn(sync, "loadStatus").mockResolvedValue();
    vi.spyOn(sync, "loadStats").mockResolvedValue();
    vi.spyOn(sync, "loadVersion").mockResolvedValue();
    vi.spyOn(sync, "checkForUpdate").mockResolvedValue();
    vi.spyOn(sync, "startPolling").mockImplementation(() => {});
    vi.spyOn(sync, "watchSession").mockImplementation(() => {});
    vi.spyOn(sessions, "load").mockResolvedValue();
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "attachSidebar").mockReturnValue(() => {});
    vi.spyOn(sessions, "loadChildSessions").mockResolvedValue();
    const cancelRouteReads = vi.spyOn(sessions, "cancelRouteReads");
    const loadMessages = vi.spyOn(messages, "loadSession").mockResolvedValue();
    const cancelMessages = vi.spyOn(messages, "cancelInFlight");
    vi.spyOn(sessionTiming, "load").mockResolvedValue();
    vi.spyOn(pins, "loadForSession").mockResolvedValue();
    vi.spyOn(usage, "fetchAll").mockResolvedValue();

    router.route = "sessions";
    router.sessionId = "session-1";
    sessions.activeSessionId = "session-1";
    component = mount(App, { target: document.body });
    await flushEffects();

    router.navigate("usage");
    await flushEffects();
    expect(cancelRouteReads).toHaveBeenCalled();
    expect(cancelMessages).toHaveBeenCalled();

    router.navigateToSession("session-1");
    await flushEffects();
    expect(loadMessages).toHaveBeenCalledTimes(2);
  });

  it("retries missing metadata when returning to the same session", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(settings, "load").mockResolvedValue();
    vi.spyOn(starred, "load").mockResolvedValue();
    vi.spyOn(sync, "loadStatus").mockResolvedValue();
    vi.spyOn(sync, "loadStats").mockResolvedValue();
    vi.spyOn(sync, "loadVersion").mockResolvedValue();
    vi.spyOn(sync, "checkForUpdate").mockResolvedValue();
    vi.spyOn(sync, "startPolling").mockImplementation(() => {});
    vi.spyOn(sync, "watchSession").mockImplementation(() => {});
    vi.spyOn(sessions, "load").mockResolvedValue();
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "attachSidebar").mockReturnValue(() => {});
    vi.spyOn(sessions, "loadChildSessions").mockResolvedValue();
    vi.spyOn(messages, "loadSession").mockResolvedValue();
    vi.spyOn(sessionTiming, "load").mockResolvedValue();
    vi.spyOn(pins, "loadForSession").mockResolvedValue();
    vi.spyOn(usage, "fetchAll").mockResolvedValue();
    const navigateToSession = vi
      .spyOn(sessions, "navigateToSession")
      .mockResolvedValue();

    sessions.sessions = [];
    sessions.activeSessionId = "session-1";
    router.route = "sessions";
    router.sessionId = "session-1";
    component = mount(App, { target: document.body });
    await flushEffects();
    expect(navigateToSession).toHaveBeenCalledTimes(1);

    router.navigate("usage");
    await flushEffects();
    router.navigateToSession("session-1");
    await flushEffects();

    expect(navigateToSession).toHaveBeenCalledTimes(2);
  });

  it("clears a stale active session entering /sessions from a non-session page (#1190)", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(settings, "load").mockResolvedValue();
    vi.spyOn(starred, "load").mockResolvedValue();
    vi.spyOn(sync, "loadStatus").mockResolvedValue();
    vi.spyOn(sync, "loadStats").mockResolvedValue();
    vi.spyOn(sync, "loadVersion").mockResolvedValue();
    vi.spyOn(sync, "checkForUpdate").mockResolvedValue();
    vi.spyOn(sync, "startPolling").mockImplementation(() => {});
    vi.spyOn(sync, "watchSession").mockImplementation(() => {});
    vi.spyOn(sessions, "load").mockResolvedValue();
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "attachSidebar").mockReturnValue(() => {});
    vi.spyOn(sessions, "loadChildSessions").mockResolvedValue();
    vi.spyOn(messages, "loadSession").mockResolvedValue();
    vi.spyOn(sessionTiming, "load").mockResolvedValue();
    vi.spyOn(pins, "loadForSession").mockResolvedValue();
    vi.spyOn(usage, "fetchAll").mockResolvedValue();
    vi.spyOn(sessions, "navigateToSession").mockResolvedValue();

    router.route = "sessions";
    router.sessionId = "session-1";
    sessions.activeSessionId = "session-1";
    component = mount(App, { target: document.body });
    await flushEffects();

    // Leave for a non-session page: the URL drops the session id but the
    // deselect guard only clears on the sessions route, so the active
    // session is intentionally preserved here.
    router.navigate("usage");
    await flushEffects();
    expect(sessions.activeSessionId).toBe("session-1");

    // Enter the bare sessions list. The session id stays null across this
    // transition, so a session-id-only dependency would not rerun; tracking
    // the route makes the effect fire and clear the stale selection.
    router.navigate("sessions");
    await flushEffects();
    expect(sessions.activeSessionId).toBeNull();
  });

  it("keeps route-first search scroll intent when entering the sessions route", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(settings, "load").mockResolvedValue();
    vi.spyOn(starred, "load").mockResolvedValue();
    vi.spyOn(sync, "loadStatus").mockResolvedValue();
    vi.spyOn(sync, "loadStats").mockResolvedValue();
    vi.spyOn(sync, "loadVersion").mockResolvedValue();
    vi.spyOn(sync, "checkForUpdate").mockResolvedValue();
    vi.spyOn(sync, "startPolling").mockImplementation(() => {});
    vi.spyOn(sync, "watchSession").mockImplementation(() => {});
    vi.spyOn(sessions, "load").mockResolvedValue();
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "attachSidebar").mockReturnValue(() => {});
    vi.spyOn(sessions, "loadChildSessions").mockResolvedValue();
    vi.spyOn(messages, "loadSession").mockResolvedValue();
    vi.spyOn(sessionTiming, "load").mockResolvedValue();
    vi.spyOn(pins, "loadForSession").mockResolvedValue();
    vi.spyOn(usage, "fetchAll").mockResolvedValue();
    const navigateToSession = vi
      .spyOn(sessions, "navigateToSession")
      .mockResolvedValue();

    router.route = "usage";
    component = mount(App, { target: document.body });
    await flushEffects();

    // Route-first search activation: URL and scroll intent are queued
    // before the deep-link effect selects the target session.
    router.navigateToSession("session-2");
    ui.scrollToOrdinal(7, "session-2");
    await flushEffects();

    expect(navigateToSession).toHaveBeenCalledWith("session-2");
    expect(ui.pendingScrollOrdinal).toBe(7);
    expect(ui.pendingScrollSession).toBe("session-2");
  });

  it("clears stale route-first scroll intent when the user selects another session", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(settings, "load").mockResolvedValue();
    vi.spyOn(starred, "load").mockResolvedValue();
    vi.spyOn(sync, "loadStatus").mockResolvedValue();
    vi.spyOn(sync, "loadStats").mockResolvedValue();
    vi.spyOn(sync, "loadVersion").mockResolvedValue();
    vi.spyOn(sync, "checkForUpdate").mockResolvedValue();
    vi.spyOn(sync, "startPolling").mockImplementation(() => {});
    vi.spyOn(sync, "watchSession").mockImplementation(() => {});
    vi.spyOn(sessions, "load").mockResolvedValue();
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "attachSidebar").mockReturnValue(() => {});
    vi.spyOn(sessions, "loadChildSessions").mockResolvedValue();
    vi.spyOn(messages, "loadSession").mockResolvedValue();
    vi.spyOn(sessionTiming, "load").mockResolvedValue();
    vi.spyOn(pins, "loadForSession").mockResolvedValue();
    vi.spyOn(usage, "fetchAll").mockResolvedValue();
    vi.spyOn(sessions, "navigateToSession").mockResolvedValue();

    router.route = "sessions";
    component = mount(App, { target: document.body });
    await flushEffects();

    // Route-first navigation to B queues its scroll intent; B's
    // hydration never lands (mocked), so the URL still names B when
    // the user selects C from the sidebar.
    router.navigateToSession("session-b");
    ui.scrollToOrdinal(7, "session-b");
    await flushEffects();
    expect(ui.pendingScrollOrdinal).toBe(7);

    sessions.activeSessionId = "session-c";
    await flushEffects();

    expect(ui.pendingScrollOrdinal).toBeNull();
    expect(ui.pendingScrollSession).toBeNull();
  });

  it("rehydrates the routed session when a sidebar rebuild drops its row", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(settings, "load").mockResolvedValue();
    vi.spyOn(starred, "load").mockResolvedValue();
    vi.spyOn(sync, "loadStatus").mockResolvedValue();
    vi.spyOn(sync, "loadStats").mockResolvedValue();
    vi.spyOn(sync, "loadVersion").mockResolvedValue();
    vi.spyOn(sync, "checkForUpdate").mockResolvedValue();
    vi.spyOn(sync, "startPolling").mockImplementation(() => {});
    vi.spyOn(sync, "watchSession").mockImplementation(() => {});
    vi.spyOn(sessions, "load").mockResolvedValue();
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "attachSidebar").mockReturnValue(() => {});
    vi.spyOn(sessions, "loadChildSessions").mockResolvedValue();
    vi.spyOn(messages, "loadSession").mockResolvedValue();
    vi.spyOn(sessionTiming, "load").mockResolvedValue();
    vi.spyOn(pins, "loadForSession").mockResolvedValue();
    vi.spyOn(usage, "fetchAll").mockResolvedValue();
    const navigateToSession = vi
      .spyOn(sessions, "navigateToSession")
      .mockResolvedValue();

    sessions.sessions = [
      {
        id: "session-1",
        project: "proj-a",
        machine: "local",
        agent: "claude",
        first_message: "hello",
        started_at: "2026-02-20T12:30:00Z",
        ended_at: "2026-02-20T12:31:00Z",
        message_count: 2,
        user_message_count: 1,
        total_output_tokens: 0,
        peak_context_tokens: 0,
        has_total_output_tokens: false,
        has_peak_context_tokens: false,
        is_automated: false,
        is_teammate: false,
        is_index_only: false,
        created_at: "2026-02-20T12:30:00Z",
      } as unknown as (typeof sessions.sessions)[number],
    ];
    sessions.activeSessionId = "session-1";
    router.route = "sessions";
    router.sessionId = "session-1";
    component = mount(App, { target: document.body });
    await flushEffects();
    expect(navigateToSession).not.toHaveBeenCalled();

    sessions.sessions = [];
    await flushEffects();

    expect(navigateToSession).toHaveBeenCalledWith("session-1");
  });

  it("renders the Data page shell for the data route", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(settings, "load").mockResolvedValue();
    vi.spyOn(starred, "load").mockResolvedValue();
    vi.spyOn(sync, "loadStatus").mockResolvedValue();
    vi.spyOn(sync, "loadStats").mockResolvedValue();
    vi.spyOn(sync, "loadVersion").mockResolvedValue();
    vi.spyOn(sync, "checkForUpdate").mockResolvedValue();
    vi.spyOn(sync, "startPolling").mockImplementation(() => {});
    vi.spyOn(sync, "watchSession").mockImplementation(() => {});
    vi.spyOn(sessions, "load").mockResolvedValue();
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "attachSidebar").mockReturnValue(() => {});

    router.route = "data";
    component = mount(App, { target: document.body });
    await flushEffects();

    const dataPage = document.querySelector(".data-page");
    expect(dataPage).not.toBeNull();
    expect(dataPage?.querySelector("h2")?.textContent).toBe("Projects");
  });

  it("treats rolling window and termination as sessions route params", () => {
    expect(SESSION_FILTER_KEYS.has("window_days")).toBe(true);
    expect(SESSION_FILTER_KEYS.has("termination")).toBe(true);
  });

  it("carries rolling intent into the sessions URL write-back", async () => {
    component = mount(App, { target: document.body });
    await tick();
    // Simulates a rolling window restored from localStorage (or applied
    // from a panel): the URL write-back must include window_days, or the
    // next route initialization re-reads the concrete dates as an
    // explicit fixed range and the rolling intent is lost after one
    // reload.
    sessions.applyPanelDateFilters(
      { date_from: "2026-03-10", date_to: "2026-04-08" },
      30,
    );
    await tick();
    await tick();
    expect(router.params["window_days"]).toBe("30");
    // The route round-trip rematerializes the window against the current
    // date — the intent surviving is the contract, not the exact bounds.
    const range = rollingRange(30);
    expect(router.params["date_from"]).toBe(range.from);
    expect(router.params["date_to"]).toBe(range.to);
  });

  it("preserves rolling window dates when writing sessions URLs", () => {
    expect(source).toContain("sessionRouteParamsForFilters(");
    expect(source).toContain("router.navigateFromSession(nextParams)");
    expect(source).toContain(
      "const newParams = sessionRouteParamsForFilters(",
    );
    expect(source).not.toContain(
      "navigateFromSession(filtersToParams(sessions.filters))",
    );
    expect(source).not.toContain(
      "const newParams = filtersToParams(sessions.filters);",
    );
  });

  it("preserves rolling window dates when entering session detail", () => {
    const syncUrlIndex = source.indexOf("// Sync active session to URL.");
    const navigateFromSessionIndex = source.indexOf(
      "router.navigateFromSession",
      syncUrlIndex,
    );
    const activeSessionBranch = source.slice(
      syncUrlIndex,
      navigateFromSessionIndex,
    );

    expect(activeSessionBranch).toContain(
      "const nextParams = sessionRouteParamsForFilters(",
    );
    expect(activeSessionBranch).toContain(
      "router.navigateToSession(activeId, nextParams)",
    );
    expect(activeSessionBranch).not.toContain(
      "router.navigateToSession(activeId);",
    );
  });

  it("preserves direct detail URL params when leaving session detail", () => {
    const syncUrlIndex = source.indexOf("// Sync active session to URL.");
    const navigateFromSessionIndex = source.indexOf(
      "router.navigateFromSession",
      syncUrlIndex,
    );
    const inactiveSessionBranch = source.slice(
      navigateFromSessionIndex - 260,
      navigateFromSessionIndex + 80,
    );

    expect(source).toContain("sessionRouteParamsForDetailExit");
    expect(inactiveSessionBranch).toContain(
      ": sessionRouteParamsForDetailExit(",
    );
    expect(inactiveSessionBranch).toContain(
      "router.navigateFromSession(nextParams)",
    );
  });

  it("updates detail URL params after explicit filter changes", () => {
    const syncUrlBlock = appSourceSlice(
      "// Sync active session to URL.",
      "// URL write-back",
    );

    expect(source).toContain(
      "let lastDetailFilterParamsSignature: string | null = $state(null);",
    );
    expect(syncUrlBlock).toContain(
      "const filterParams = sessionFilterRouteParams();",
    );
    expect(syncUrlBlock).toContain(
      "lastDetailFilterParamsSignature !== null &&",
    );
    expect(syncUrlBlock).toContain("router.replaceParams(nextParams);");
    expect(syncUrlBlock).toContain(
      "lastDetailFilterParamsSignature = filterParamsSignature;",
    );
  });

  it("does not preserve stale detail params after filter changes", () => {
    const syncUrlBlock = appSourceSlice(
      "// Sync active session to URL.",
      "// URL write-back",
    );

    expect(syncUrlBlock).toContain("const filterChangedOnDetail =");
    expect(syncUrlBlock).toContain(
      "filterChangedOnDetail\n          ? sessionRouteParamsForFilters(",
    );
    expect(syncUrlBlock).toContain(
      ": sessionRouteParamsForDetailExit(",
    );
  });

  it("clears stored yoke when session date params are removed while analytics is unmounted", () => {
    const syncUrlBlock = appSourceSlice(
      "// Sync active session to URL.",
      "// URL write-back",
    );
    const writeBackBlock = appSourceSlice(
      "// URL write-back",
      "function showAbout",
    );

    expect(source).toContain("function clearYokeForClearedSessionDates");
    expect(source).toContain("sessionDateIntentCleared(");
    expect(source).toContain("yokedDates.clear();");
    expect(syncUrlBlock).toContain(
      "clearYokeForClearedSessionDates(nextParams);",
    );
    expect(writeBackBlock).toContain(
      "clearYokeForClearedSessionDates(newParams);",
    );
  });

  it("clears detail filter signatures outside session detail routes", () => {
    const syncUrlBlock = appSourceSlice(
      "// Sync active session to URL.",
      "// URL write-back",
    );

    expect(syncUrlBlock).toContain(
      'if (router.route !== "sessions") {\n        lastDetailFilterParamsSignature = null;',
    );
    expect(syncUrlBlock).toContain(
      "if (currentUrlSessionId === null) {\n          lastDetailFilterParamsSignature = null;",
    );
  });

  it("restores the full recently deleted batch from the undo toast", () => {
    const undoBlock = appSourceSlice(
      "{#if sessions.recentlyDeleted.length > 0}",
      "</div>\n{/if}",
    );

    expect(undoBlock).toContain(
      "await sessions.restoreRecentlyDeleted(last);",
    );
    expect(undoBlock).not.toContain("await sessions.restoreSession(last.id);");
  });
});
function message(
  ordinal: number,
  role: Message["role"],
  isSystem = false,
) {
  return {
    kind: "message" as const,
    ordinals: [ordinal],
    message: { role, is_system: isSystem } as Message,
  };
}

describe("findUserPromptOrdinal", () => {
  const items = [
    message(1, "user"),
    message(2, "assistant"),
    { kind: "tool-group" as const, ordinals: [3], messages: [], timestamp: "" },
    message(4, "user"),
  ];

  it("moves among visible user messages in chronological order", () => {
    expect(findUserPromptOrdinal(items, 1, 1, true)).toBe(4);
    expect(findUserPromptOrdinal(items, 2, 1, true)).toBe(4);
    expect(findUserPromptOrdinal(items, 3, -1, true)).toBe(1);
    expect(findUserPromptOrdinal(items, null, 1, true)).toBe(1);
    expect(findUserPromptOrdinal(items, null, -1, true)).toBe(4);
    expect(findUserPromptOrdinal(items, 99, 1, true)).toBe(1);
    expect(findUserPromptOrdinal(items, 99, -1, true)).toBe(4);
    expect(findUserPromptOrdinal(items.slice(1, 3), 2, 1, true)).toBeUndefined();
  });

  it("keeps chronological directions when newest-first reorders rows", () => {
    expect(findUserPromptOrdinal(items, 1, 1, true)).toBe(4);
    expect(findUserPromptOrdinal(items, 4, -1, true)).toBe(1);
  });

  it("skips user rows when only their code segment is visible", () => {
    const codeOnlyUser = {
      id: 5,
      role: "user",
      content: "```ts\nconst hiddenPrompt = true;\n```",
      has_tool_use: false,
      content_length: 36,
    } as Message;
    expect(hasVisibleSegments(codeOnlyUser, (type) => type === "code")).toBe(true);

    expect(findUserPromptOrdinal([
      message(1, "assistant"),
      { kind: "message", ordinals: [5], message: codeOnlyUser },
    ], 1, 1, false)).toBeUndefined();
  });

  it("skips system boundaries with a user role", () => {
    expect(findUserPromptOrdinal([
      message(1, "user"),
      message(2, "user", true),
      message(3, "user"),
    ], 1, 1, true)).toBe(3);
  });
});

describe("App analytics date navigation", () => {
  it("materializes and publishes rolling detail dates before loading", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-07-10T12:00:00"));
    const datesAtSessionLoad: Array<{
      shared: { from: string; to: string } | null;
      filters: { from: string; to: string };
    }> = [];
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(settings, "load").mockResolvedValue();
    vi.spyOn(starred, "load").mockResolvedValue();
    vi.spyOn(sync, "loadStatus").mockResolvedValue();
    vi.spyOn(sync, "loadStats").mockResolvedValue();
    vi.spyOn(sync, "loadVersion").mockResolvedValue();
    vi.spyOn(sync, "checkForUpdate").mockResolvedValue();
    vi.spyOn(sync, "startPolling").mockImplementation(() => {});
    vi.spyOn(sync, "watchSession").mockImplementation(() => {});
    vi.spyOn(sessions, "load").mockImplementation(() => {
      const shared = yokedDates.seedForPanel();
      datesAtSessionLoad.push({
        shared: shared ? { from: shared.from, to: shared.to } : null,
        filters: {
          from: sessions.filters.dateFrom,
          to: sessions.filters.dateTo,
        },
      });
      return Promise.resolve();
    });
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "attachSidebar").mockReturnValue(() => {});
    vi.spyOn(sessions, "loadChildSessions").mockResolvedValue();
    vi.spyOn(sessions, "navigateToSession").mockImplementation(async (id) => {
      sessions.activeSessionId = id;
    });
    vi.spyOn(messages, "loadSession").mockResolvedValue();
    vi.spyOn(sessionTiming, "load").mockResolvedValue();
    vi.spyOn(pins, "loadForSession").mockResolvedValue();
    vi.spyOn(analytics, "fetchAll").mockResolvedValue();
    vi.spyOn(analytics, "fetchSignalsForInsights").mockResolvedValue();
    vi.spyOn(insights, "load").mockResolvedValue();
    vi.spyOn(usage, "fetchAll").mockResolvedValue();

    window.history.replaceState(null, "", "/insights");
    router.route = "insights";
    router.params = {};
    router.sessionId = null;
    yokedDates.setEnabled(true);
    yokedDates.updateFromPanel({
      from: "2026-05-01",
      to: "2026-05-31",
      mode: "fixed",
    });

    component = mount(App, { target: document.body });
    await flushEffects();

    router.navigateToSession("session-1", {
      msg: "42",
      window_days: "30",
      date_from: "2026-01-01",
      date_to: "2026-01-30",
    });
    await flushEffects();

    expect(datesAtSessionLoad[0]).toEqual({
      shared: { from: "2026-06-11", to: "2026-07-10" },
      filters: { from: "2026-06-11", to: "2026-07-10" },
    });
    expect(yokedDates.seedForPanel()).toMatchObject({
      from: "2026-06-11",
      to: "2026-07-10",
      mode: "rolling",
      windowDays: 30,
    });
    expect(router.params).toMatchObject({
      msg: "42",
      window_days: "30",
      date_from: "2026-06-11",
      date_to: "2026-07-10",
    });
    expect(ui.selectedOrdinal).toBe(42);

    router.navigate("usage");
    await flushEffects();

    expect(usage.isPinned).toBe(false);
    expect(usage.windowDays).toBe(30);
    expect(usage.from).toBe("2026-06-11");
    expect(usage.to).toBe("2026-07-10");
  });

  it("applies an enabled shared range when entering session detail", async () => {
    const sessionLoadDates: Array<{
      dateFrom: string;
      dateTo: string;
    }> = [];
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(settings, "load").mockResolvedValue();
    vi.spyOn(starred, "load").mockResolvedValue();
    vi.spyOn(sync, "loadStatus").mockResolvedValue();
    vi.spyOn(sync, "loadStats").mockResolvedValue();
    vi.spyOn(sync, "loadVersion").mockResolvedValue();
    vi.spyOn(sync, "checkForUpdate").mockResolvedValue();
    vi.spyOn(sync, "startPolling").mockImplementation(() => {});
    vi.spyOn(sync, "watchSession").mockImplementation(() => {});
    vi.spyOn(sessions, "load").mockImplementation(() => {
      sessionLoadDates.push({
        dateFrom: sessions.filters.dateFrom,
        dateTo: sessions.filters.dateTo,
      });
      return Promise.resolve();
    });
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "attachSidebar").mockReturnValue(() => {});
    vi.spyOn(sessions, "navigateToSession").mockImplementation(async (id) => {
      sessions.activeSessionId = id;
    });
    vi.spyOn(analytics, "fetchAll").mockResolvedValue();
    vi.spyOn(analytics, "fetchSignalsForInsights").mockResolvedValue();
    vi.spyOn(insights, "load").mockResolvedValue();

    window.history.replaceState(null, "", "/insights");
    router.route = "insights";
    router.params = {};
    router.sessionId = null;
    yokedDates.setEnabled(true);
    yokedDates.updateFromPanel({
      from: "2026-05-01",
      to: "2026-05-31",
      mode: "fixed",
    });

    component = mount(App, { target: document.body });
    await flushEffects();
    sessionLoadDates.length = 0;

    router.navigateToSession("session-1");
    await flushEffects();

    expect(sessionLoadDates[0]).toEqual({
      dateFrom: "2026-05-01",
      dateTo: "2026-05-31",
    });
    expect(router.sessionId).toBe("session-1");
    expect(router.params.date_from).toBe("2026-05-01");
    expect(router.params.date_to).toBe("2026-05-31");
  });

  it("applies an enabled shared range to the first Sessions load", async () => {
    const sessionLoadFilters: Array<{
      project: string;
      dateFrom: string;
      dateTo: string;
    }> = [];
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(settings, "load").mockResolvedValue();
    vi.spyOn(starred, "load").mockResolvedValue();
    vi.spyOn(sync, "loadStatus").mockResolvedValue();
    vi.spyOn(sync, "loadStats").mockResolvedValue();
    vi.spyOn(sync, "loadVersion").mockResolvedValue();
    vi.spyOn(sync, "checkForUpdate").mockResolvedValue();
    vi.spyOn(sync, "startPolling").mockImplementation(() => {});
    vi.spyOn(sessions, "load").mockImplementation(() => {
      sessionLoadFilters.push({
        project: sessions.filters.project,
        dateFrom: sessions.filters.dateFrom,
        dateTo: sessions.filters.dateTo,
      });
      return Promise.resolve();
    });
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "attachSidebar").mockReturnValue(() => {});
    vi.spyOn(analytics, "fetchAll").mockResolvedValue();
    vi.spyOn(analytics, "fetchSignalsForInsights").mockResolvedValue();
    vi.spyOn(insights, "load").mockResolvedValue();

    window.history.replaceState(null, "", "/insights");
    router.route = "insights";
    router.params = {};
    router.sessionId = null;
    analyticsPageDates.retain(
      "sessions",
      {
        from: "2026-01-01",
        to: "2026-01-31",
        mode: "fixed",
      },
      true,
    );
    yokedDates.setEnabled(true);
    yokedDates.updateFromPanel({
      from: "2026-05-01",
      to: "2026-05-31",
      mode: "fixed",
    });

    component = mount(App, { target: document.body });
    await flushEffects();
    sessionLoadFilters.length = 0;

    router.navigate("sessions", { project: "project-alpha" });
    await flushEffects();

    expect(sessionLoadFilters[0]).toEqual({
      project: "project-alpha",
      dateFrom: "2026-05-01",
      dateTo: "2026-05-31",
    });
  });

  it("restores a retained rolling Sessions range without pinning it", async () => {
    const sessionLoadDates: Array<{
      dateFrom: string;
      dateTo: string;
    }> = [];
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-07-10T12:00:00"));
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(settings, "load").mockResolvedValue();
    vi.spyOn(starred, "load").mockResolvedValue();
    vi.spyOn(sync, "loadStatus").mockResolvedValue();
    vi.spyOn(sync, "loadStats").mockResolvedValue();
    vi.spyOn(sync, "loadVersion").mockResolvedValue();
    vi.spyOn(sync, "checkForUpdate").mockResolvedValue();
    vi.spyOn(sync, "startPolling").mockImplementation(() => {});
    vi.spyOn(sessions, "load").mockImplementation(() => {
      sessionLoadDates.push({
        dateFrom: sessions.filters.dateFrom,
        dateTo: sessions.filters.dateTo,
      });
      return Promise.resolve();
    });
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "attachSidebar").mockReturnValue(() => {});
    vi.spyOn(analytics, "fetchAll").mockResolvedValue();
    vi.spyOn(analytics, "fetchSignalsForInsights").mockResolvedValue();
    vi.spyOn(insights, "load").mockResolvedValue();

    window.history.replaceState(null, "", "/sessions");
    router.route = "sessions";
    router.params = {};
    router.sessionId = null;
    analytics.applyRollingWindow(365);
    yokedDates.setEnabled(false);

    component = mount(App, { target: document.body });
    await flushEffects();
    await selectRelativeRange(30);

    router.navigate("insights");
    await flushEffects();
    sessionLoadDates.length = 0;
    const dateFiltersAtRouteReplace: Array<{
      dateFrom: string;
      dateTo: string;
    }> = [];
    const replaceParams = router.replaceParams.bind(router);
    vi.spyOn(router, "replaceParams").mockImplementation((params) => {
      if (params.window_days === "30") {
        dateFiltersAtRouteReplace.push({
          dateFrom: sessions.filters.dateFrom,
          dateTo: sessions.filters.dateTo,
        });
      }
      replaceParams(params);
    });
    window.history.replaceState(null, "", "/sessions");
    window.dispatchEvent(new PopStateEvent("popstate"));
    await flushEffects();

    expect(dateFiltersAtRouteReplace[0]).toEqual({
      dateFrom: "2026-06-11",
      dateTo: "2026-07-10",
    });
    expect(sessionLoadDates[0]).toEqual({
      dateFrom: "2026-06-11",
      dateTo: "2026-07-10",
    });
    expect(analytics.isPinned).toBe(false);
    expect(analytics.windowDays).toBe(30);
    expect(router.params.window_days).toBe("30");
    expect(router.params.date_from).toBe("2026-06-11");
    expect(router.params.date_to).toBe("2026-07-10");

    sessions.filters.date = "";
    sessions.filters.dateFrom = "";
    sessions.filters.dateTo = "";
    // Clearing dates now also means clearing the rolling intent — the
    // store-level clear paths (clearSessionFilters, setProjectFilter) do
    // both, and the URL write-back re-adds window_days otherwise.
    sessions.dateFiltersWindowDays = null;
    router.params = {};
    await flushEffects();

    expect(router.params).toEqual({});
  });

  it("shares a retained Insights range after linking is enabled", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-07-10T12:00:00"));
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(settings, "load").mockResolvedValue();
    vi.spyOn(starred, "load").mockResolvedValue();
    vi.spyOn(sync, "loadStatus").mockResolvedValue();
    vi.spyOn(sync, "loadStats").mockResolvedValue();
    vi.spyOn(sync, "loadVersion").mockResolvedValue();
    vi.spyOn(sync, "checkForUpdate").mockResolvedValue();
    vi.spyOn(sync, "startPolling").mockImplementation(() => {});
    vi.spyOn(sessions, "load").mockResolvedValue();
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "attachSidebar").mockReturnValue(() => {});
    vi.spyOn(analytics, "fetchAll").mockResolvedValue();
    vi.spyOn(analytics, "fetchSignalsForInsights").mockResolvedValue();
    vi.spyOn(insights, "load").mockResolvedValue();
    vi.spyOn(usage, "fetchAll").mockResolvedValue();

    window.history.replaceState(null, "", "/insights");
    router.route = "insights";
    router.params = {};
    router.sessionId = null;
    analytics.applyRollingWindow(365);
    yokedDates.setEnabled(false);

    component = mount(App, { target: document.body });
    await flushEffects();
    await selectRelativeRange(90);

    router.navigate("settings");
    await flushEffects();
    yokedDates.setEnabled(true);
    router.navigate("insights");
    await flushEffects();
    router.navigate("usage");
    await flushEffects();

    expect(usage.isPinned).toBe(false);
    expect(usage.windowDays).toBe(90);
    expect(usage.from).toBe("2026-04-12");
    expect(usage.to).toBe("2026-07-10");
  });
});
