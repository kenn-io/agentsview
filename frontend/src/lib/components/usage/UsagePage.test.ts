import { describe, expect, it } from "vite-plus/test";
import source from "./UsagePage.svelte?raw";

describe("UsagePage refresh behavior", () => {
  it("does not auto-refresh usage scans from SSE updates", () => {
    expect(source).not.toContain("subscribeDebounced");
    expect(source).not.toContain("REFRESH_MS");
    // SSE only flags new data; the periodic refetch lives in RefreshControl.
    expect(source).toContain("usage.markNewData");
    expect(source).toContain("events.subscribe");
  });

  it("delegates the refresh affordance and scheduler to RefreshControl", () => {
    expect(source).toContain("<RefreshControl");
    expect(source).toContain("usage.lastUpdatedAt");
    expect(source).toContain('label="Refresh usage data"');
    expect(source).toContain('title="Refresh"');
    // The scheduler, label tick, and icon now live in the shared component.
    expect(source).not.toContain("REFRESH_LABEL_INTERVAL_MS");
    expect(source).not.toContain("formatRefreshAge");
    expect(source).not.toContain("RefreshCwIcon");
    expect(source).not.toContain("setInterval");
  });

  it("shows relative last-updated status without ambiguous badges", () => {
    expect(source).not.toContain("formatUpdatedAt");
    expect(source).not.toContain("usage.hasNewData");
    expect(source).not.toContain("New data");
    expect(source).not.toContain(".new-data");
  });

  it("treats termination as a usage URL session filter", () => {
    expect(source).toContain('"termination",');
    expect(source).toContain("filtersToParams(sessions.filters)");
  });

  it("keeps refresh progress out of content layout flow", () => {
    const queryProgress =
      source.match(/\.query-progress\s*{[^}]+}/)?.[0] ?? "";

    expect(queryProgress).toContain("position: absolute");
    expect(queryProgress).toContain("left: 0;");
    expect(queryProgress).toContain("right: 0;");
    expect(queryProgress).not.toContain("position: sticky");
    expect(queryProgress).not.toContain("margin:");
  });

  it("updates shared yoke state from usage range selections", () => {
    expect(source).toContain("<RangePicker");
    expect(source).toContain("function applyRange");
    expect(source).toContain("updateYokeFromUsage");
    expect(source).toContain("yokedDates.updateFromPanel");
  });

  it("seeds bare usage URLs from shared yoked dates", () => {
    expect(source).toContain("const seed = yokedDates.seedForPanel()");
    expect(source).toContain("applyUsagePanelDate(state)");
    expect(source).toContain("usage.applyRollingWindow");
    expect(source).toContain("usage.applyDateRange");
  });

  it("hydrates supported termination filters from usage URLs", () => {
    const filterKeysIndex = source.indexOf("const SESSION_FILTER_KEYS");
    const urlInitIndex = source.indexOf("let urlInitRan", filterKeysIndex);
    const filterKeysBlock = source.slice(filterKeysIndex, urlInitIndex);

    expect(filterKeysBlock).toContain('"termination"');
  });

  it("does not hydrate session-only date filters from usage URLs", () => {
    const filterKeysIndex = source.indexOf("const SESSION_FILTER_KEYS");
    const urlInitIndex = source.indexOf("let urlInitRan", filterKeysIndex);
    const filterKeysBlock = source.slice(filterKeysIndex, urlInitIndex);

    expect(filterKeysBlock).not.toContain('"date"');
    expect(filterKeysBlock).not.toContain('"date_from"');
    expect(filterKeysBlock).not.toContain('"date_to"');
  });

  it("sanitizes mixed usage URL session params before hydrating", () => {
    const initStart = source.indexOf("if (hasSessionFilterKeys)");
    const initEnd = source.indexOf("if (hasDateParam)", initStart);
    const initBlock = source.slice(initStart, initEnd);

    expect(source).toContain("function usageSupportedSessionParams");
    expect(initBlock).toContain(
      "parseFiltersFromParams(supportedSessionParams)",
    );
    expect(initBlock).toContain(
      "sessions.initFromParams(supportedSessionParams)",
    );
    expect(initBlock).not.toContain("parseFiltersFromParams(params)");
    expect(initBlock).not.toContain("sessions.initFromParams(params)");
  });
});
