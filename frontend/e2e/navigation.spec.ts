import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

test.describe("Navigation", () => {
  let sp: SessionsPage;

  test.beforeEach(async ({ page }) => {
    sp = new SessionsPage(page);
    await sp.goto();
  });

  test("keyboard ] navigates to next session", async () => {
    await sp.sessionItems.first().click();
    await expect(sp.sessionItems.first()).toHaveClass(/active/);

    await sp.pressNextSessionShortcut();
    await expect(sp.sessionItems.nth(1)).toHaveClass(/active/);
  });

  test("keyboard [ navigates to previous session", async () => {
    await sp.sessionItems.nth(1).click();
    await expect(sp.sessionItems.nth(1)).toHaveClass(/active/);

    await sp.pressPreviousSessionShortcut();
    await expect(sp.sessionItems.first()).toHaveClass(/active/);
  });

  test("analytics page shows when no session selected", async () => {
    await expect(sp.analyticsPage).toBeVisible();
    await expect(sp.analyticsToolbar).toBeVisible();
    await expect(sp.exportBtn).toContainText("Export CSV");
  });

  test("search result for an unlisted session keeps the breadcrumb project (#1190)", async ({ page }) => {
    // The target stays off the sidebar index page, as with a paginated
    // or filtered index in production. Search still finds it; opening
    // the result hydrates it by ID, and the index reload that entering
    // the sessions route triggers must not drop the hydrated row after
    // the detail fetch lands.
    const sessionId = "test-session-duration-subagent-1";
    await page.goto("/usage");
    await page.route("**/api/v1/sessions/sidebar-index**", async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      body.sessions = body.sessions.filter(
        (s: { id: string }) => s.id !== sessionId,
      );
      await new Promise((resolve) => setTimeout(resolve, 500));
      await route.fulfill({ response, json: body });
    });

    await page.keyboard.press("ControlOrMeta+k");
    const input = page.locator(".palette-input");
    await expect(input).toBeVisible();
    await input.fill("synchronous DB read");

    const result = page.locator(".palette-item", {
      hasText: "synchronous DB read",
    }).first();
    await expect(result).toBeVisible();
    const indexReloaded = page.waitForResponse("**/api/v1/sessions/sidebar-index**");
    await result.click();

    await expect(page).toHaveURL(new RegExp(`/sessions/${sessionId}`));
    await indexReloaded;
    await expect(page.locator(".breadcrumb-current")).toHaveText(
      /project-duration/,
    );
  });

  test("subagent cost rollup", async ({ page }, testInfo) => {
    await page.route("**/api/v1/sessions/*/usage**", async (route) => {
      const rollup = new URL(route.request().url()).searchParams.get("rollup");
      const complete = route.request().url().includes("test-session-mixed-content-7");
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          session_id: "test-session-mixed-content-7",
          agent: "claude",
          project: "project",
          total_output_tokens: 0,
          peak_context_tokens: 0,
          has_token_data: false,
          cost_usd: 1,
          has_cost: true,
          models: [],
          unpriced_models: [],
          breakdown_count: 0,
          breakdown: [],
          server_running: true,
          ...(rollup === "true"
            ? {
                has_rollup_cost: complete,
                ...(complete ? { rollup_cost_usd: 3 } : {}),
                rollup_subagent_count: 1,
              }
            : {}),
        }),
      });
    });

    const selected = page.locator('.session-item[data-session-id="test-session-mixed-content-7"]');
    await selected.click();
    for (const width of [1280, 768, 400]) {
      await page.setViewportSize({ width, height: 800 });
      await expect(page.locator(".cost-badge")).toContainText("Total");
      await expect(page.locator(".cost-badge")).toContainText("$3.00");
      await expect(page.locator(".cost-badge")).toHaveAttribute(
        "title",
        "Total cost including 1 subagent",
      );
      await page.screenshot({
        path: testInfo.outputPath(`subagent-cost-rollup-${width}.png`),
      });
    }

    await page.setViewportSize({ width: 1280, height: 800 });
    const fallback = page.locator(
      '.session-item:visible:not([data-session-id="test-session-mixed-content-7"])',
    ).first();
    await fallback.click();
    await expect(page.locator(".cost-badge")).toContainText("$1.00");
    await expect(page.locator(".cost-badge")).not.toContainText("Total");
  });

  test("Shift+J and Shift+K navigate visible user prompts", async ({ page }, testInfo) => {
    const sessionId = "test-session-mixed-content-7";
    const session = page.locator(`.session-item[data-session-id="${sessionId}"]`);
    await expect(session).toHaveCount(1);
    await session.click();
    await expect(session).toHaveClass(/active/);
    await expect(sp.scroller).toHaveAttribute("data-messages-session-id", sessionId);
    await expect(sp.messageRows).toHaveCount(6);

    const users = sp.messageRows.filter({
      has: page.locator(".message.is-user"),
    });
    await users.first().click();
    await expect(users.first()).toHaveClass(/selected/);

    const assistants = sp.messageRows.filter({
      has: page.locator(".message:not(.is-user)"),
    });
    await page.keyboard.press("j");
    await expect(assistants.first()).toHaveClass(/selected/);
    await users.first().click();

    await page.keyboard.press("Shift+J");
    await expect(users.nth(1)).toHaveClass(/selected/);
    await page.keyboard.press("Shift+K");
    await expect(users.first()).toHaveClass(/selected/);

    await users.nth(1).click();
    await sp.toggleSortOrder();
    await expect(users.nth(1)).toHaveClass(/selected/);
    await page.keyboard.press("Shift+J");
    await expect(users.nth(2)).toHaveClass(/selected/);
    await users.nth(1).click();
    await page.keyboard.press("Shift+K");
    await expect(users.nth(0)).toHaveClass(/selected/);

    await page.keyboard.press("?");
    await expect(page.getByText("Next user prompt")).toBeVisible();
    await expect(page.getByText("Previous user prompt")).toBeVisible();

    const shortcutsModal = page.getByRole("dialog", {
      name: "Keyboard Shortcuts",
    });
    for (const width of [1280, 768, 400]) {
      await page.setViewportSize({ width, height: 800 });
      const box = await shortcutsModal.boundingBox();
      expect(box).not.toBeNull();
      expect(box!.x).toBeGreaterThanOrEqual(0);
      expect(box!.x + box!.width).toBeLessThanOrEqual(width);
      await page.screenshot({
        path: testInfo.outputPath(`shortcuts-${width}.png`),
      });
    }
  });
});
