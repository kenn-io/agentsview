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
