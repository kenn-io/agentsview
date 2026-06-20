import { describe, expect, it } from "vite-plus/test";
import source from "./InsightsPage.svelte?raw";

describe("InsightsPage sidebar filter sync", () => {
  it("syncs the automated-session scope from the sidebar", () => {
    // Insight scope derives from analytics.includeAutomated, so the
    // sidebar->insights sync must mirror the analytics page: read the
    // sidebar toggle, map it to all/human, and write both fields.
    const normalized = source.replace(/\s+/g, " ");
    expect(source).toContain("sessions.filters.includeAutomated");
    expect(normalized).toContain(
      'headerIncludeAutomated ? "all" : "human"',
    );
    expect(source).toContain(
      "analytics.includeAutomated = headerIncludeAutomated",
    );
    expect(source).toContain(
      "analytics.automatedScope = headerAutomatedScope",
    );
  });

  it("refetches when the automated scope changes", () => {
    // includeAutomated and automatedScope must take part in the change
    // detection that triggers the refetch, not just be assigned.
    const normalized = source.replace(/\s+/g, " ");
    expect(normalized).toContain(
      "untrack(() => analytics.includeAutomated) !== headerIncludeAutomated",
    );
    expect(normalized).toContain(
      "untrack(() => analytics.automatedScope) !== headerAutomatedScope",
    );
    expect(source).toContain("fetchInsightSignals()");
  });
});

describe("InsightsPage date yoke controls", () => {
  it("updates and seeds shared yoke state from the unified range picker", () => {
    expect(source).toContain("<RangePicker");
    expect(source).toContain("updateYokeFromInsights");
    expect(source).toContain("seedInsightsYoke");
    expect(source).toContain("rangeToPanelDate(seed)");
  });

  it("lets insight URL dates override stored yoke dates", () => {
    expect(source).toContain("insightParamsToPanelDate(router.params)");
    expect(source).toContain("hasInsightDateParams(router.params)");
    expect(source).toContain("paramsWithInsightDate");
    expect(source).toContain("rangeToInsightParams(range)");
  });

  it("preserves relative range selections as rolling yoke state", () => {
    const applyIndex = source.indexOf("function applyRange");
    const parseIndex = source.indexOf(
      "function parseInsightWindowDays",
      applyIndex,
    );
    const applyBlock = source.slice(applyIndex, parseIndex);

    expect(source).toContain('mode: "rolling"');
    expect(source).toContain("windowDays: sel.days");
    expect(applyBlock).toContain("analytics.setRollingWindow(sel.days)");
    expect(applyBlock).toContain("updateYokeFromInsights(state)");
  });

  it("preserves rolling window intent in insight URLs", () => {
    expect(source).toContain('const INSIGHTS_WINDOW_PARAM = "window_days"');
    expect(source).toContain("parseInsightWindowDays");
    expect(source).toContain("rollingRange(windowDays)");
    expect(source).toContain("delete nextParams[key]");
    expect(source).toContain("paramsWithInsightDate");
  });

  it("refreshes rolling insight URL/yoke bounds after signal fetches", () => {
    const fetchIndex = source.indexOf("function fetchInsightSignals");
    const nextHandlerIndex = source.indexOf(
      "\n\n  function handleProjectChange",
      fetchIndex,
    );
    const fetchBlock = source.slice(fetchIndex, nextHandlerIndex);

    expect(fetchBlock).toContain("analytics.fetchSignalsForInsights()");
    expect(fetchBlock).toContain("updateYokeFromInsights(state)");
  });

  it("routes automated scope changes through the insight refresh wrapper", () => {
    const handlerIndex = source.indexOf(
      "function handleAutomatedScopeChange",
    );
    const nextHandlerIndex = source.indexOf(
      "\n\n  function handlePromptChange",
      handlerIndex,
    );
    const handlerBlock = source.slice(handlerIndex, nextHandlerIndex);

    expect(handlerBlock).toContain("fetchInsightSignals()");
    expect(handlerBlock).not.toContain("analytics.setAutomatedScope");
  });
});
