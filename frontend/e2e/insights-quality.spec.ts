import { expect, test } from "@playwright/test";

const cannedInsight = {
  id: 42,
  type: "llm_canned",
  date_from: "2026-05-01",
  date_to: "2026-05-26",
  project: null,
  agent: "claude",
  model: "test-model",
  prompt: null,
  content: [
    "# Prompt Maturity",
    "",
    "Deterministic score distribution: 10 scored sessions, average 92.",
    "",
    "> Generated recommendation text. Deterministic health scores and signal rows were not modified.",
  ].join("\n"),
  kind: "prompt_maturity_review",
  schema_version: "llm_insight.v1",
  template_id: "prompt_maturity_review",
  template_version: "v1",
  aggregate_hash: "abcdef1234567890",
  cache_key: "cache:test",
  cache_status: "hit",
  provenance_json: "{}",
  structured_json: "{}",
  created_at: "2026-05-26T12:00:00Z",
};

test.describe("Insights quality rollout", () => {
  test.beforeEach(async ({ page }) => {
    await page.route("**/api/v1/projects*", (route) =>
      route.fulfill({ json: { projects: [] } }),
    );
    await page.route("**/api/v1/agents*", (route) =>
      route.fulfill({ json: { agents: ["claude", "codex"] } }),
    );
    await page.route("**/api/v1/sessions*", (route) =>
      route.fulfill({ json: { sessions: [], total: 0 } }),
    );
    await page.route("**/api/v1/sync/status", (route) =>
      route.fulfill({ json: { last_sync: null, stats: null } }),
    );
    await page.route("**/api/v1/stats*", (route) =>
      route.fulfill({
        json: {
          total_sessions: 0,
          total_messages: 0,
          total_user_messages: 0,
          total_assistant_messages: 0,
          total_tool_calls: 0,
          total_projects: 0,
          total_machines: 0,
          total_agents: 0,
          by_agent: [],
          by_project: [],
        },
      }),
    );
    await page.route("**/api/v1/update/check", (route) =>
      route.fulfill({
        json: {
          update_available: false,
          current_version: "test",
        },
      }),
    );
  });

  test("renders saved deterministic quality recommendation metadata", async ({
    page,
  }) => {
    await page.addInitScript(() => {
      Object.defineProperty(navigator, "clipboard", {
        value: {
          writeText: async (text: string) => {
            (
              window as unknown as { __copiedInsightLink?: string }
            ).__copiedInsightLink = text;
          },
        },
        configurable: true,
      });
    });
    await page.route("**/api/v1/version", (route) =>
      route.fulfill({
        json: { version: "test", commit: "test", read_only: false },
      }),
    );
    await page.route("**/api/v1/insights", (route) =>
      route.fulfill({ json: { insights: [cannedInsight] } }),
    );

    await page.goto("/insights");

    const archive = page.getByRole("region", {
      name: "Generated Insights Archive",
    });
    const savedInsight = archive.getByRole("button", {
      name: /Prompt Maturity global/,
    });
    await expect(
      page.getByRole("heading", { name: "Quality Patterns" }),
    ).toBeVisible();
    await expect(savedInsight).toBeVisible();
    await savedInsight.click();
    await expect(page).toHaveURL(/\/insights\?insight=42$/);

    await expect(
      page.locator(".generated-detail .badge", {
        hasText: "Prompt Maturity",
      }),
    ).toBeVisible();
    await expect(page.getByText("cache hit")).toBeVisible();
    await expect(page.getByText("template v1")).toBeVisible();
    await expect(page.getByText("aggregate abcdef123456")).toBeVisible();
    await expect(
      page
        .locator(".generated-detail")
        .getByRole("heading", { name: "Prompt Maturity" }),
    ).toBeVisible();
    await expect(
      page.getByText(
        "Deterministic score distribution: 10 scored sessions, average 92.",
      ),
    ).toBeVisible();
    await expect(
      page.getByText("Deterministic health scores and signal rows were not modified."),
    ).toBeVisible();

    const copyLink = page.getByRole("button", {
      name: "Copy generated insight link",
    });
    await expect(copyLink).toBeVisible();
    await copyLink.click();
    await expect(
      page.getByRole("button", {
        name: "Copied generated insight link",
      }),
    ).toBeVisible();
    const copied = await page.evaluate(
      () =>
        (window as unknown as { __copiedInsightLink?: string })
          .__copiedInsightLink,
    );
    expect(copied).toBe(`${new URL(page.url()).origin}/insights?insight=42`);

    await page.goto("/insights?insight=42");
    await expect(
      page
        .locator(".generated-detail")
        .getByRole("heading", { name: "Prompt Maturity" }),
    ).toBeVisible();
  });

  test("keeps generation disabled in read-only mode", async ({ page }) => {
    await page.route("**/api/v1/version", (route) =>
      route.fulfill({
        json: { version: "test", commit: "test", read_only: true },
      }),
    );
    await page.route("**/api/v1/insights", (route) =>
      route.fulfill({ json: { insights: [] } }),
    );
    await page.route("**/api/v1/insights/generate", (route) =>
      route.fulfill({
        status: 500,
        body: "generate should stay disabled in read-only mode",
      }),
    );

    await page.goto("/insights");

    await expect(
      page.getByRole("heading", { name: "Generated Insights Archive" }),
    ).toBeVisible();
    await expect(
      page.getByText("No generated insights saved."),
    ).toBeVisible();
    const generate = page
      .getByRole("region", { name: "Generated Insights Archive" })
      .getByRole("button", { name: "Generate" });
    await expect(generate).toHaveAttribute(
      "title",
      "Generation is disabled in read-only mode",
    );
    await expect(generate).toBeDisabled();
  });
});
