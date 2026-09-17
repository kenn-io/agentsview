import { expect, test } from "@playwright/test";
import {
  createMockSessions,
  handleSessionsRoute,
  sessionsRoutePattern,
} from "./helpers/mock-sessions";

const sessions = createMockSessions(1, "eur", () => "currency-project");
const session = sessions[0]!;
const projectKey = "pl1:sha256:currency-project";
const secondProjectKey = "pl1:sha256:other-project";
const usageTotal = 15_000_000;
const sessionCost = 10_000_000;

const settings = {
  agent_dirs: {},
  chart_palette: "agentsview",
  disabled_agents: [],
  github_configured: false,
  host: "127.0.0.1",
  port: 8080,
  read_only: false,
  require_auth: false,
  session_providers: [],
  terminal: { mode: "auto" },
};

const sessionUsage = {
  session_id: session.id,
  agent: session.agent,
  project: session.project,
  total_output_tokens: 25,
  peak_context_tokens: 100,
  has_token_data: true,
  cost: { microdollars: 10_000_000 },
  has_cost: true,
  models: ["test-model"],
  unpriced_models: [],
  breakdown_count: 0,
  breakdown: [],
  server_running: true,
};

const usageSummary = {
  from: "2026-09-16",
  to: "2026-09-17",
  projects: {},
  totals: {
    inputTokens: 1_000,
    outputTokens: 500,
    cacheCreationTokens: 100,
    cacheReadTokens: 50,
    totalCost: { microdollars: usageTotal },
    cacheSavings: { microdollars: 2_000_000 },
  },
  daily: [
    {
      date: "2026-09-16",
      inputTokens: 1_000,
      outputTokens: 500,
      cacheCreationTokens: 100,
      cacheReadTokens: 50,
      totalCost: { microdollars: usageTotal },
      modelsUsed: ["test-model", "second-model"],
      projectBreakdowns: [
        {
          project_key: projectKey,
          project: "currency-project",
          inputTokens: 700,
          outputTokens: 350,
          cacheCreationTokens: 70,
          cacheReadTokens: 35,
          cost: { microdollars: sessionCost },
        },
        {
          project_key: secondProjectKey,
          project: "other-project",
          inputTokens: 300,
          outputTokens: 150,
          cacheCreationTokens: 30,
          cacheReadTokens: 15,
          cost: { microdollars: 5_000_000 },
        },
      ],
      modelBreakdowns: [
        {
          modelName: "test-model",
          inputTokens: 700,
          outputTokens: 350,
          cacheCreationTokens: 70,
          cacheReadTokens: 35,
          cost: { microdollars: sessionCost },
        },
        {
          modelName: "second-model",
          inputTokens: 300,
          outputTokens: 150,
          cacheCreationTokens: 30,
          cacheReadTokens: 15,
          cost: { microdollars: 5_000_000 },
        },
      ],
      agentBreakdowns: [],
      machineBreakdowns: [],
    },
  ],
  projectTotals: [
    {
      project_key: projectKey,
      project: "currency-project",
      inputTokens: 700,
      outputTokens: 350,
      cacheCreationTokens: 70,
      cacheReadTokens: 35,
      cost: { microdollars: sessionCost },
    },
    {
      project_key: secondProjectKey,
      project: "other-project",
      inputTokens: 300,
      outputTokens: 150,
      cacheCreationTokens: 30,
      cacheReadTokens: 15,
      cost: { microdollars: 5_000_000 },
    },
  ],
  modelTotals: [
    {
      model: "test-model",
      inputTokens: 700,
      outputTokens: 350,
      cacheCreationTokens: 70,
      cacheReadTokens: 35,
      cost: { microdollars: sessionCost },
    },
    {
      model: "second-model",
      inputTokens: 300,
      outputTokens: 150,
      cacheCreationTokens: 30,
      cacheReadTokens: 15,
      cost: { microdollars: 5_000_000 },
    },
  ],
  agentTotals: [],
  sessionCounts: {
    total: 1,
    byProject: { [projectKey]: 1, [secondProjectKey]: 0 },
    byAgent: {},
  },
  cacheStats: {
    cacheReadTokens: 50,
    cacheCreationTokens: 100,
    uncachedInputTokens: 1_000,
    outputTokens: 500,
    hitRate: 0.5,
    savingsVsUncached: { microdollars: 2_000_000 },
  },
};

const topSessions = [
  {
    sessionId: session.id,
    displayName: "EUR proof session",
    agent: session.agent,
    project: session.project,
    startedAt: session.started_at,
    inputTokens: 700,
    outputTokens: 350,
    cacheCreationTokens: 70,
    cacheReadTokens: 35,
    totalTokens: 1_085,
    cost: { microdollars: sessionCost },
  },
];

const pairwiseComparison = {
  left: {
    totalCost: { microdollars: sessionCost },
    inputTokens: 700,
    outputTokens: 350,
    cacheCreationTokens: 70,
    cacheReadTokens: 35,
    totalTokens: 1_085,
    sessionCount: 1,
    costPerSession: { microdollars: sessionCost },
    tokensPerSession: 1_085,
  },
  right: {
    totalCost: { microdollars: 5_000_000 },
    inputTokens: 300,
    outputTokens: 150,
    cacheCreationTokens: 30,
    cacheReadTokens: 15,
    totalTokens: 465,
    sessionCount: 1,
    costPerSession: { microdollars: 5_000_000 },
    tokensPerSession: 465,
  },
  deltas: {
    totalCostDelta: { microdollars: -5_000_000 },
    totalCostDeltaRatio: -0.5,
    inputTokensDelta: -400,
    inputTokensDeltaRatio: -0.5714,
    outputTokensDelta: -200,
    outputTokensDeltaRatio: -0.5714,
    cacheCreationDelta: -40,
    cacheCreationDeltaRatio: -0.5714,
    cacheReadDelta: -20,
    cacheReadDeltaRatio: -0.5714,
    totalTokensDelta: -620,
    totalTokensDeltaRatio: -0.5714,
    sessionCountDelta: 0,
    sessionCountDeltaRatio: 0,
    costPerSessionDelta: { microdollars: -5_000_000 },
    costPerSessionRatio: -0.5,
    tokensPerSessionDelta: -620,
    tokensPerSessionRatio: -0.5714,
  },
};

const activityReport = {
  report_id: "currency-activity-proof",
  schema_version: 1,
  timezone: "UTC",
  range_start: "2026-09-16T00:00:00Z",
  range_end: "2026-09-17T00:00:00Z",
  bucket_unit: "hour",
  bucket_seconds: 3_600,
  bucket_count: 1,
  elapsed_bucket_count: 1,
  effective_end: "2026-09-17T00:00:00Z",
  partial: false,
  as_of: null,
  peak: { agents: 1, at: "2026-09-16T12:00:00Z" },
  interactive_peak: { agents: 1, at: "2026-09-16T12:00:00Z" },
  subagent_peak: { agents: 0, at: null },
  automated_peak: { agents: 0, at: null },
  totals: {
    active_minutes: 10,
    idle_minutes: 0,
    agent_minutes: 10,
    sessions: 1,
    untimed_sessions: 0,
    distinct_projects: 1,
    distinct_models: 1,
    output_tokens: 350,
    cost: { microdollars: sessionCost },
    subagent_agent_minutes: 0,
    automated_agent_minutes: 0,
    interactive_agent_minutes: 10,
    subagent_cost: { microdollars: 0 },
    automated_cost: { microdollars: 0 },
    interactive_cost: { microdollars: sessionCost },
    automated_sessions: 0,
    interactive_sessions: 1,
    subagent_sessions: 0,
  },
  buckets: [
    {
      start: "2026-09-16T12:00:00Z",
      end: "2026-09-16T13:00:00Z",
      max_agents: 1,
      agent_minutes: 10,
      output_tokens: 350,
      input_tokens: 700,
      cost: { microdollars: sessionCost },
      interactive_at_peak: 1,
      subagent_at_peak: 0,
      automated_at_peak: 0,
      max_interactive_agents: 1,
      max_subagent_agents: 0,
      max_automated_agents: 0,
    },
  ],
  by_project: [
    {
      key: "currency-project",
      project_key: projectKey,
      agent_minutes: 10,
      cost: { microdollars: sessionCost },
      interactive_agent_minutes: 10,
      subagent_agent_minutes: 0,
      automated_agent_minutes: 0,
      interactive_cost: { microdollars: sessionCost },
      subagent_cost: { microdollars: 0 },
      automated_cost: { microdollars: 0 },
    },
  ],
  by_model: [],
  by_agent: [],
  by_session: [
    {
      session_id: session.id,
      title: "EUR proof session",
      project: "currency-project",
      project_key: projectKey,
      agent: session.agent,
      primary_model: "test-model",
      models: ["test-model"],
      agent_minutes: 10,
      cost: { microdollars: sessionCost },
      output_tokens: 350,
      first_active: "2026-09-16T12:00:00Z",
      last_active: "2026-09-16T12:10:00Z",
      timing_quality: "timed",
      is_automated: false,
      is_subagent: false,
    },
  ],
  sessions_total: 1,
  projects: {},
};

test.describe("Currency display", () => {
  test.beforeEach(async ({ page }) => {
    await page.route("**/api/v1/**", (route) =>
      route.fulfill({ status: 404, json: { error: "not mocked" } }),
    );
    await page.route(sessionsRoutePattern, handleSessionsRoute([{ sessions, project: null }]));
    await page.route("**/api/v1/settings", (route) => route.fulfill({ json: settings }));
    await page.route(`**/api/v1/sessions/${session.id}/usage*`, (route) =>
      route.fulfill({ json: sessionUsage }),
    );
    await page.route(`**/api/v1/sessions/${session.id}/messages*`, (route) =>
      route.fulfill({ json: { messages: [], count: 0 } }),
    );
    await page.route(`**/api/v1/sessions/${session.id}/children*`, (route) =>
      route.fulfill({ json: { sessions: [], total: 0 } }),
    );
    await page.route(`**/api/v1/sessions/${session.id}/directory*`, (route) =>
      route.fulfill({ json: { path: "/tmp/currency-project" } }),
    );
    await page.route("**/api/v1/usage/summary*", (route) =>
      route.fulfill({ json: usageSummary }),
    );
    await page.route("**/api/v1/usage/top-sessions*", (route) =>
      route.fulfill({ json: topSessions }),
    );
    await page.route("**/api/v1/usage/comparison*", (route) =>
      route.fulfill({
        json: {
          priorFrom: "2026-09-15",
          priorTo: "2026-09-16",
          priorTotalCost: { microdollars: 5_000_000 },
          deltaPct: 2,
        },
      }),
    );
    await page.route("**/api/v1/usage/pairwise-comparison*", (route) =>
      route.fulfill({ json: pairwiseComparison }),
    );
    await page.route("**/api/v1/activity/report*", (route) =>
      route.fulfill({ json: activityReport }),
    );
    await page.route("**/api/v1/projects*", (route) =>
      route.fulfill({ json: { projects: [] } }),
    );
    await page.route("**/api/v1/agents*", (route) =>
      route.fulfill({ json: { agents: [] } }),
    );
    await page.route("**/api/v1/machines*", (route) =>
      route.fulfill({ json: { machines: [] } }),
    );
    await page.route("**/api/v1/sync/status", (route) =>
      route.fulfill({ json: { last_sync: null, stats: null } }),
    );
    await page.route("**/api/v1/version", (route) =>
      route.fulfill({
        json: {
          api_version: 1,
          build_date: "",
          commit: "test",
          data_version: 1,
          insight_generation_available: false,
          read_only: false,
          version: "test",
        },
      }),
    );
    await page.route("**/api/v1/stats*", (route) =>
      route.fulfill({
        json: {
          earliest_session: null,
          machine_count: 1,
          message_count: 0,
          project_count: 1,
          session_count: 1,
        },
      }),
    );
    await page.route("**/api/v1/update/check", (route) =>
      route.fulfill({ json: { update_available: false } }),
    );
  });

  test("applies a manual EUR rate and retains it after reload", async ({ page }) => {
    await page.goto("/settings");
    const nav = page.getByRole("navigation", { name: "Settings" });
    await expect(nav).toBeVisible();
    await nav.locator("button", { hasText: "Currency" }).click();
    await expect(page.getByRole("heading", { name: "Currency" })).toBeVisible();

    await page.locator('button[title="Display currency"]').click();
    await page.getByRole("option", { name: "EUR", exact: true }).click();
    await page.getByLabel("EUR per USD").fill("0.90");
    await expect(page.getByText("1 USD = 0.90 EUR", { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Apply", exact: true }).click();

    await expect
      .poll(() => page.evaluate(() => localStorage.getItem("agentsview-cost-display")))
      .toBe(JSON.stringify({ currency: "EUR", eurPerUsd: 0.9 }));

    await page.goto(`/sessions/${encodeURIComponent(session.id)}`);
    await expect(page.locator(".cost-badge")).toHaveText("€9.00");

    await page.reload();
    await expect(page.locator(".cost-badge")).toHaveText("€9.00");

    await page.goto("/usage");
    await expect(page.locator(".usage-page")).toBeVisible();
    await expect(page.locator(".usage-content .summary-cards .card-value").first()).toBeVisible();
    await expect(page.locator(".usage-content .summary-cards .card.featured .card-value")).toHaveText(
      "€13.50",
    );
    await expect(page.locator(".attribution-panel .rail-cost").first()).toHaveText("€9.00");
    await expect(page.locator(".top-sessions-container .session-cost").first()).toHaveText("€9.00");
    await expect(page.locator(".savings-callout")).toContainText("€1.80");
    await expect(page.locator(".pairwise-panel")).toContainText("€9.00");
    await expect(page.locator(".pairwise-panel")).toContainText("€4.50");
    await expect(page.locator(".pairwise-panel")).toContainText("-€4.50");

    await page.goto("/activity");
    await expect(page.locator(".activity-page")).toBeVisible();
    await expect(page.locator(".activity-content .summary-cards .card").filter({ hasText: "Total Cost" }).locator(
      ".card-value",
    )).toHaveText("€9.00");
    await expect(page.locator(".sessions-table .col-cost").first()).toHaveText("€9.00");
    await page.locator(".breakdowns .metric-btn").filter({ hasText: "Cost" }).click();
    await expect(page.locator(".breakdowns .bar-value").first()).toHaveText("€9.00");
  });
});
