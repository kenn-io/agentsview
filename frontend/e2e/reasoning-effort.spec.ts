import { test, expect, type Page } from "@playwright/test";

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: "wait" });
});

const SESSION = "test-session-duration-showcase";

async function routeMessages(page: Page, effort?: string) {
  await page.route("**/api/v1/sessions/*/messages*", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    const messages = body.messages ?? [];
    for (const message of messages) {
      if (message.role !== "assistant") continue;
      if (effort) message.reasoning_effort = effort;
      else delete message.reasoning_effort;
    }
    await route.fulfill({
      response,
      json: { ...body, messages },
    });
  });
}

test("renders recorded reasoning effort beside the main model", async ({ page }, testInfo) => {
  await routeMessages(page, "high");

  for (const width of [1280, 768, 400]) {
    await page.setViewportSize({ width, height: 600 });
    await page.goto(`/sessions/${SESSION}`);
    const badge = page.locator(".model-badge");
    const effort = badge.locator(".model-badge__effort");
    await expect(effort).toHaveText("high", {
      timeout: 5_000,
    });
    await expect(page.locator(".actions-wrapper")).toBeVisible();
    // The header rewraps as late session data arrives, so read every box
    // from one layout and retry until the header settles.
    await expect(async () => {
      const boxes = await page.evaluate(() => {
        const rect = (selector: string) => {
          const found = document.querySelector(selector)?.getBoundingClientRect();
          return found ? { left: found.left, right: found.right } : null;
        };
        return {
          badge: rect(".model-badge"),
          effort: rect(".model-badge__effort"),
          actions: rect(".actions-wrapper"),
        };
      });
      expect(boxes.badge).not.toBeNull();
      expect(boxes.effort).not.toBeNull();
      expect(boxes.actions).not.toBeNull();
      const { badge: badgeBox, effort: effortBox, actions: actionsBox } = boxes;
      expect(effortBox!.left).toBeGreaterThanOrEqual(badgeBox!.left);
      expect(effortBox!.right).toBeLessThanOrEqual(badgeBox!.right);
      expect(badgeBox!.right).toBeLessThanOrEqual(width);
      expect(actionsBox!.right).toBeLessThanOrEqual(width);
    }).toPass({ timeout: 5_000 });
    await page.screenshot({
      path: testInfo.outputPath(`reasoning-effort-after-${width}.png`),
      fullPage: false,
    });
  }
});

test("keeps the model badge when reasoning effort is absent", async ({ page }, testInfo) => {
  await routeMessages(page);
  for (const width of [1280, 768, 400]) {
    await page.setViewportSize({ width, height: 600 });
    await page.goto(`/sessions/${SESSION}`);

    const badge = page.locator(".model-badge");
    await expect(badge).toContainText("claude-sonnet-4-20250514", {
      timeout: 5_000,
    });
    await expect(badge.locator(".model-badge__effort")).toHaveCount(0);
    await expect(page.locator(".actions-wrapper")).toBeVisible();
    const badgeBox = await badge.boundingBox();
    const actionsBox = await page.locator(".actions-wrapper").boundingBox();
    expect(badgeBox).not.toBeNull();
    expect(actionsBox).not.toBeNull();
    if (badgeBox && actionsBox) {
      expect(badgeBox.x + badgeBox.width).toBeLessThanOrEqual(width);
      expect(actionsBox.x + actionsBox.width).toBeLessThanOrEqual(width);
    }
    await page.screenshot({
      path: testInfo.outputPath(`reasoning-effort-absent-${width}.png`),
      fullPage: false,
    });
  }
});
