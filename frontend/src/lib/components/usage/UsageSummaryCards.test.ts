// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import UsageSummaryCards from "./UsageSummaryCards.svelte";
import { usage } from "../../stores/usage.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import type { UsageSummaryResponse } from "../../api/types/usage.js";

const ZERO_TOTALS = {
  inputTokens: 0,
  outputTokens: 0,
  cacheCreationTokens: 0,
  cacheReadTokens: 0,
  totalCost: 0,
};

function summary(
  over: Partial<UsageSummaryResponse>,
): UsageSummaryResponse {
  return {
    from: "2026-06-01",
    to: "2026-06-30",
    totals: ZERO_TOTALS,
    daily: [],
    projectTotals: [],
    modelTotals: [],
    agentTotals: [],
    sessionCounts: { total: 0, byProject: {}, byAgent: {} },
    cacheStats: { hitRate: 0, savingsVsUncached: 0 },
    matchingSessions: 0,
    ...over,
  } as UsageSummaryResponse;
}

async function render(): Promise<{ host: HTMLElement; cleanup: () => void }> {
  const host = document.createElement("div");
  document.body.appendChild(host);
  const app = mount(UsageSummaryCards, { target: host });
  await tick();
  return {
    host,
    cleanup: () => {
      unmount(app);
      host.remove();
    },
  };
}

afterEach(() => {
  usage.summary = null;
  usage.errors.summary = null;
  sessions.filters.agent = "";
});

describe("UsageSummaryCards no-token-data hint", () => {
  it("shows the Copilot hint when filtered to Copilot sessions with zero usage", async () => {
    sessions.filters.agent = "copilot";
    usage.summary = summary({ matchingSessions: 2 });
    const { host, cleanup } = await render();
    expect(host.textContent).toContain("Copilot");
    expect(host.textContent).toContain("monthly request quota");
    cleanup();
  });

  it("shows the hint for an all-Copilot comma-separated filter", async () => {
    sessions.filters.agent = "copilot,vscode-copilot";
    usage.summary = summary({ matchingSessions: 1 });
    const { host, cleanup } = await render();
    expect(host.textContent).toContain("monthly request quota");
    cleanup();
  });

  it("shows no hint when no matching sessions exist (empty window)", async () => {
    sessions.filters.agent = "copilot";
    usage.summary = summary({ matchingSessions: 0 });
    const { host, cleanup } = await render();
    expect(host.textContent).not.toContain("monthly request quota");
    cleanup();
  });

  it("shows no hint when the filter is not a Copilot agent", async () => {
    sessions.filters.agent = "claude";
    usage.summary = summary({ matchingSessions: 5 });
    const { host, cleanup } = await render();
    expect(host.textContent).not.toContain("monthly request quota");
    cleanup();
  });

  it("shows no hint when there is no agent filter", async () => {
    sessions.filters.agent = "";
    usage.summary = summary({ matchingSessions: 5 });
    const { host, cleanup } = await render();
    expect(host.textContent).not.toContain("monthly request quota");
    cleanup();
  });

  it("shows no hint when Copilot has token/cost data", async () => {
    sessions.filters.agent = "copilot";
    usage.summary = summary({
      totals: { ...ZERO_TOTALS, totalCost: 1.5 },
      matchingSessions: 2,
    });
    const { host, cleanup } = await render();
    expect(host.textContent).not.toContain("monthly request quota");
    cleanup();
  });
});
