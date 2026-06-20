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
      route.fulfill({
        json: {
          agents: [
            { name: "claude", session_count: 8 },
            { name: "codex", session_count: 5 },
            { name: "hermes", session_count: 3 },
          ],
        },
      }),
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
    await archive.getByTitle("Select generator").click();
    await expect(
      archive.getByRole("option", { name: "Codex", exact: true }),
    ).toBeVisible();
    await expect(
      archive.getByRole("option", { name: "Copilot", exact: true }),
    ).toBeVisible();
    await expect(
      archive.getByRole("option", { name: /Codex \(/ }),
    ).toHaveCount(0);
    await expect(
      archive.getByRole("option", { name: /Hermes/ }),
    ).toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(savedInsight).toBeVisible();
    await savedInsight.click();
    await expect(page).toHaveURL(/\/insights\?.*insight=42/);
    const selectedInsightUrl = new URL(page.url());
    expect(selectedInsightUrl.pathname).toBe("/insights");
    expect(selectedInsightUrl.searchParams.get("insight")).toBe("42");
    expect(selectedInsightUrl.searchParams.get("window_days")).toBe("365");
    expect(selectedInsightUrl.searchParams.get("date_from")).toMatch(
      /^\d{4}-\d{2}-\d{2}$/,
    );
    expect(selectedInsightUrl.searchParams.get("date_to")).toMatch(
      /^\d{4}-\d{2}-\d{2}$/,
    );

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
    await expect(
      page.getByRole("button", { name: "Delete generated insight" }),
    ).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Delete", exact: true }),
    ).toHaveCount(0);

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
    const copiedUrl = new URL(copied!);
    expect(copiedUrl.origin).toBe(selectedInsightUrl.origin);
    expect(copiedUrl.pathname).toBe("/insights");
    expect(copiedUrl.searchParams.get("insight")).toBe("42");
    expect(copiedUrl.searchParams.get("window_days")).toBe("365");
    expect(copiedUrl.searchParams.get("date_from")).toBe(
      selectedInsightUrl.searchParams.get("date_from"),
    );
    expect(copiedUrl.searchParams.get("date_to")).toBe(
      selectedInsightUrl.searchParams.get("date_to"),
    );

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

  test("retries failed generated insight tasks", async ({ page }) => {
    let generateCalls = 0;
    await page.route("**/api/v1/version", (route) =>
      route.fulfill({
        json: { version: "test", commit: "test", read_only: false },
      }),
    );
    await page.route("**/api/v1/insights", (route) =>
      route.fulfill({ json: { insights: [] } }),
    );
    await page.route("**/api/v1/insights/generate", (route) => {
      generateCalls += 1;
      if (generateCalls === 1) {
        return route.fulfill({
          contentType: "text/event-stream",
          body: [
            "event: error",
            'data: {"message":"generated insight failed validation: unknown envelope evidence_ref: usage:cache_behavior"}',
            "",
            "",
          ].join("\n"),
        });
      }
      return route.fulfill({
        contentType: "text/event-stream",
        body: [
          "event: done",
          `data: ${JSON.stringify({
            ...cannedInsight,
            id: 43,
            kind: "model_cost_review",
            template_id: "model_cost_review",
          })}`,
          "",
          "",
        ].join("\n"),
      });
    });

    await page.goto("/insights");

    const archive = page.getByRole("region", {
      name: "Generated Insights Archive",
    });
    await archive.getByTitle("Select template").click();
    await archive.getByRole("option", { name: "Model and Cost" }).click();
    await archive.getByRole("button", { name: "Generate" }).click();

    await expect(
      archive.getByText("generated insight failed validation"),
    ).toBeVisible();
    await expect(
      archive.getByRole("button", { name: "Dismiss failed generation" }),
    ).toBeVisible();

    await archive.getByRole("button", { name: "Retry" }).click();

    await expect(
      archive.getByRole("button", { name: /Model and Cost global/ }),
    ).toBeVisible();
    expect(generateCalls).toBe(2);
  });
});
