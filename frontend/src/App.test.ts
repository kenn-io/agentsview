import { describe, expect, it } from "vite-plus/test";
import { hasVisibleSegments } from "./lib/utils/content-parser.js";
import { findUserPromptOrdinal } from "./App.svelte";
import {
  getLastRecentlyDeletedBatch,
  resolveSessionRouteSync,
  resolveSessionRouteWriteBack,
  shouldBaselineReadProgress,
} from "./app-logic.js";
import { SESSION_FILTER_KEYS } from "./lib/stores/sessionRouteParams.js";
import type { Message } from "./lib/api/types.js";

describe("App session URL date state", () => {
  it("baselines read progress from successful session metadata", () => {
    expect(shouldBaselineReadProgress({
      activeSessionId: "s1",
      messageSessionId: "s1",
      loading: false,
      initialLoadSucceeded: true,
      latestDisplayOrdinal: 7,
    })).toBe(true);
    expect(shouldBaselineReadProgress({
      activeSessionId: "s1",
      messageSessionId: "s1",
      loading: false,
      initialLoadSucceeded: true,
      latestDisplayOrdinal: undefined,
    })).toBe(false);
    expect(shouldBaselineReadProgress({
      activeSessionId: "s1",
      messageSessionId: "s2",
      loading: false,
      initialLoadSucceeded: true,
      latestDisplayOrdinal: 7,
    })).toBe(false);
  });

  it("treats rolling window and termination as sessions route params", () => {
    expect(SESSION_FILTER_KEYS.has("window_days")).toBe(true);
    expect(SESSION_FILTER_KEYS.has("termination")).toBe(true);
  });

  it("preserves rolling window dates when writing sessions URLs", () => {
    const action = resolveSessionRouteWriteBack({
      route: "sessions",
      currentUrlSessionId: null,
      currentParams: {
        window_days: "30",
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      },
      filterParams: {
        termination: "success",
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      },
      now: new Date("2026-06-20T12:00:00Z"),
    });

    expect(action).toMatchObject({
      kind: "replace-params",
      nextParams: {
        window_days: "30",
        termination: "success",
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      },
      clearYoke: false,
    });
  });

  it("preserves rolling window dates when entering session detail", () => {
    const action = resolveSessionRouteSync({
      route: "sessions",
      activeSessionId: "session-1",
      currentUrlSessionId: null,
      currentParams: {
        window_days: "30",
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      },
      filterParams: {
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      },
      lastDetailFilterParamsSignature: null,
      now: new Date("2026-06-20T12:00:00Z"),
    });

    expect(action).toMatchObject({
      kind: "navigate-to-session",
      sessionId: "session-1",
      nextParams: {
        window_days: "30",
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      },
      nextSignature: JSON.stringify({
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      }),
      clearYoke: false,
    });
  });

  it("preserves direct detail URL params when leaving session detail", () => {
    const action = resolveSessionRouteSync({
      route: "sessions",
      activeSessionId: null,
      currentUrlSessionId: "session-1",
      currentParams: {
        window_days: "30",
        termination: "error",
        msg: "99",
      },
      filterParams: {
        project: "agentsview",
      },
      lastDetailFilterParamsSignature: null,
    });

    expect(action).toMatchObject({
      kind: "navigate-from-session",
      nextParams: {
        window_days: "30",
        termination: "error",
      },
      clearYoke: false,
    });
  });

  it("updates detail URL params after explicit filter changes", () => {
    const filterParams = {
      termination: "success",
      date_from: "2026-05-22",
      date_to: "2026-06-20",
    };
    const action = resolveSessionRouteSync({
      route: "sessions",
      activeSessionId: "session-1",
      currentUrlSessionId: "session-1",
      currentParams: {
        window_days: "30",
        termination: "error",
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      },
      filterParams,
      lastDetailFilterParamsSignature: JSON.stringify({
        termination: "error",
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      }),
      now: new Date("2026-06-20T12:00:00Z"),
    });

    expect(action).toMatchObject({
      kind: "replace-params",
      nextParams: {
        window_days: "30",
        termination: "success",
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      },
      nextSignature: JSON.stringify(filterParams),
      clearYoke: false,
    });
  });

  it("does not preserve stale detail params after filter changes", () => {
    const filterParams = {
      date_from: "2026-06-10",
      date_to: "2026-06-20",
    };
    const action = resolveSessionRouteSync({
      route: "sessions",
      activeSessionId: null,
      currentUrlSessionId: "session-1",
      currentParams: {
        msg: "12",
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      },
      filterParams,
      lastDetailFilterParamsSignature: JSON.stringify({
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      }),
      now: new Date("2026-06-20T12:00:00Z"),
    });

    expect(action).toMatchObject({
      kind: "navigate-from-session",
      nextParams: filterParams,
      clearYoke: false,
    });
  });

  it("clears stored yoke when session date params are removed while analytics is unmounted", () => {
    const syncAction = resolveSessionRouteSync({
      route: "sessions",
      activeSessionId: null,
      currentUrlSessionId: "session-1",
      currentParams: {
        window_days: "30",
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      },
      filterParams: {
        project: "agentsview",
      },
      lastDetailFilterParamsSignature: JSON.stringify({
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      }),
    });
    const writeBackAction = resolveSessionRouteWriteBack({
      route: "sessions",
      currentUrlSessionId: null,
      currentParams: {
        window_days: "30",
        date_from: "2026-05-22",
        date_to: "2026-06-20",
      },
      filterParams: {
        agent: "claude",
      },
    });

    expect(syncAction.clearYoke).toBe(true);
    expect(writeBackAction).toMatchObject({
      kind: "replace-params",
      nextParams: { agent: "claude" },
      clearYoke: true,
    });
  });

  it("clears detail filter signatures outside session detail routes", () => {
    expect(resolveSessionRouteSync({
      route: "usage",
      activeSessionId: "session-1",
      currentUrlSessionId: "session-1",
      currentParams: {},
      filterParams: {},
      lastDetailFilterParamsSignature: "old",
    })).toEqual({
      kind: "reset-signature",
      nextSignature: null,
      clearYoke: false,
    });
    expect(resolveSessionRouteSync({
      route: "sessions",
      activeSessionId: null,
      currentUrlSessionId: null,
      currentParams: {},
      filterParams: {},
      lastDetailFilterParamsSignature: "old",
    })).toEqual({
      kind: "reset-signature",
      nextSignature: null,
      clearYoke: false,
    });
  });

  it("restores the full recently deleted batch from the undo toast", () => {
    const batch = {
      key: 2,
      ids: ["session-2", "session-3"],
      timer: {} as ReturnType<typeof setTimeout>,
    };

    expect(getLastRecentlyDeletedBatch([
      {
        key: 1,
        ids: ["session-1"],
        timer: {} as ReturnType<typeof setTimeout>,
      },
      batch,
    ])).toBe(batch);
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
