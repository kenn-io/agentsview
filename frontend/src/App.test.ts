import { describe, expect, it } from "vite-plus/test";
import type { Message } from "./lib/api/types.js";
import { hasVisibleSegments } from "./lib/utils/content-parser.js";
import { findUserPromptOrdinal } from "./App.svelte";
import sourceRaw from "./App.svelte?raw";
import { SESSION_FILTER_KEYS } from "./lib/stores/sessionRouteParams.js";

const source = sourceRaw.replace(/\r\n/g, "\n");

function appSourceSlice(startMarker: string, endMarker: string): string {
  const start = source.indexOf(startMarker);
  expect(start).toBeGreaterThan(-1);
  const end = source.indexOf(endMarker, start);
  expect(end).toBeGreaterThan(start);
  return source.slice(start, end);
}

describe("App session URL date state", () => {
  it("treats rolling window and termination as sessions route params", () => {
    expect(SESSION_FILTER_KEYS.has("window_days")).toBe(true);
    expect(SESSION_FILTER_KEYS.has("termination")).toBe(true);
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
    expect(syncUrlBlock).toContain("const filterParams = filtersToParams(");
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

    expect(source).toContain("import { yokedDates");
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
