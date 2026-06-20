import { describe, expect, it } from "vite-plus/test";
import source from "./App.svelte?raw";
import { SESSION_FILTER_KEYS } from "./lib/stores/sessionRouteParams.js";

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
      "\n\n  // URL write-back",
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
      "\n\n  // URL write-back",
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
      "\n\n  // URL write-back",
    );
    const writeBackBlock = appSourceSlice(
      "// URL write-back",
      "\n\n  function showAbout",
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
      "\n\n  // URL write-back",
    );

    expect(syncUrlBlock).toContain(
      'if (router.route !== "sessions") {\n        lastDetailFilterParamsSignature = null;',
    );
    expect(syncUrlBlock).toContain(
      "if (currentUrlSessionId === null) {\n          lastDetailFilterParamsSignature = null;",
    );
  });
});
