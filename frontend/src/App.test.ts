import { describe, expect, it } from "vite-plus/test";
import source from "./App.svelte?raw";

describe("App session URL date state", () => {
  it("treats rolling window and termination as sessions route params", () => {
    const filterKeysIndex = source.indexOf("const SESSION_FILTER_KEYS");
    const hasFilterParamsIndex = source.indexOf(
      "function hasFilterParams",
      filterKeysIndex,
    );
    const filterKeysBlock = source.slice(
      filterKeysIndex,
      hasFilterParamsIndex,
    );

    expect(filterKeysBlock).toContain('"window_days"');
    expect(filterKeysBlock).toContain('"termination"');
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

    expect(source).toContain("function sessionRouteParamsForDetailExit");
    expect(source).toContain("function currentSessionRouteParams");
    expect(inactiveSessionBranch).toContain(
      ": sessionRouteParamsForDetailExit(",
    );
    expect(inactiveSessionBranch).toContain(
      "router.navigateFromSession(nextParams)",
    );
  });

  it("prefers direct detail URL params over saved filters on exit", () => {
    const helperStart = source.indexOf(
      "function sessionRouteParamsForDetailExit",
    );
    const helperEnd = source.indexOf(
      "\n\n  let lastDetailFilterParamsSignature",
      helperStart,
    );
    const helperBlock = source.slice(helperStart, helperEnd);

    expect(helperBlock).toContain(
      "const currentRouteParams = currentSessionRouteParams(currentParams);",
    );
    expect(helperBlock).toContain(
      "if (hasFilterParams(currentRouteParams)) return currentRouteParams;",
    );
  });

  it("updates detail URL params after explicit filter changes", () => {
    const syncUrlIndex = source.indexOf("// Sync active session to URL.");
    const syncUrlEnd = source.indexOf(
      "\n\n  // Compare only filter keys",
      syncUrlIndex,
    );
    const syncUrlBlock = source.slice(syncUrlIndex, syncUrlEnd);

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
    const syncUrlIndex = source.indexOf("// Sync active session to URL.");
    const syncUrlEnd = source.indexOf(
      "\n\n  // Compare only filter keys",
      syncUrlIndex,
    );
    const syncUrlBlock = source.slice(syncUrlIndex, syncUrlEnd);

    expect(syncUrlBlock).toContain("const filterChangedOnDetail =");
    expect(syncUrlBlock).toContain(
      "filterChangedOnDetail\n          ? sessionRouteParamsForFilters(",
    );
    expect(syncUrlBlock).toContain(
      ": sessionRouteParamsForDetailExit(",
    );
  });

  it("clears detail filter signatures outside session detail routes", () => {
    const syncUrlIndex = source.indexOf("// Sync active session to URL.");
    const syncUrlEnd = source.indexOf(
      "\n\n  // Compare only filter keys",
      syncUrlIndex,
    );
    const syncUrlBlock = source.slice(syncUrlIndex, syncUrlEnd);

    expect(syncUrlBlock).toContain(
      'if (router.route !== "sessions") {\n        lastDetailFilterParamsSignature = null;',
    );
    expect(syncUrlBlock).toContain(
      "if (currentUrlSessionId === null) {\n          lastDetailFilterParamsSignature = null;",
    );
  });
});
