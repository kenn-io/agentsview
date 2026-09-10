import { test, expect, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";

const SESSION = "test-session-duration-showcase";
const renderLintSource = process.env.PR_RENDER_LINT_PATH
  ? readFileSync(process.env.PR_RENDER_LINT_PATH, "utf8")
  : null;

async function routeMessages(page: Page, effort?: string) {
  await page.route(
    "**/api/v1/sessions/*/messages*",
    async (route) => {
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
    },
  );
}

async function assertRenderLint(page: Page, width: number) {
  if (!renderLintSource) return;
  const violations = await page.evaluate((source) => {
    const browserSource = source.replace(/\nmodule\.exports[\s\S]*$/, "");
    const collectRenderViolations = new Function(
      `${browserSource}\nreturn collectRenderViolations;`,
    )() as (
      scopeSelector: string,
      options: { overlapTolerancePx: number },
    ) => unknown[];
    return collectRenderViolations(".model-badge", {
      overlapTolerancePx: 6,
    });
  }, renderLintSource);
  console.log(`render-lint width=${width}: ${JSON.stringify(violations)}`);
  expect(violations).toEqual([]);
}

test("renders recorded reasoning effort beside the main model", async ({
  page,
}, testInfo) => {
  await routeMessages(page, "high");

  for (const width of [1280, 768, 400]) {
    await page.setViewportSize({ width, height: 600 });
    await page.goto(`/sessions/${SESSION}`);
    const badge = page.locator(".model-badge");
    const effort = badge.locator(".model-badge__effort");
    await expect(effort).toHaveText("high", {
      timeout: 5_000,
    });
    await assertRenderLint(page, width);
    await expect(page.locator(".actions-wrapper")).toBeVisible();
    const badgeBox = await badge.boundingBox();
    const effortBox = await effort.boundingBox();
    const actionsBox = await page.locator(".actions-wrapper").boundingBox();
    expect(badgeBox).not.toBeNull();
    expect(actionsBox).not.toBeNull();
    expect(effortBox).not.toBeNull();
    if (badgeBox && effortBox && actionsBox) {
      expect(effortBox.x).toBeGreaterThanOrEqual(badgeBox.x);
      expect(effortBox.x + effortBox.width).toBeLessThanOrEqual(
        badgeBox.x + badgeBox.width,
      );
      expect(badgeBox.x + badgeBox.width).toBeLessThanOrEqual(width);
      expect(actionsBox.x + actionsBox.width).toBeLessThanOrEqual(width);
    }
    await page.screenshot({
      path: testInfo.outputPath(`reasoning-effort-after-${width}.png`),
      fullPage: false,
    });
  }
});

test("keeps the model badge when reasoning effort is absent", async ({
  page,
}, testInfo) => {
  await routeMessages(page);
  for (const width of [1280, 768, 400]) {
    await page.setViewportSize({ width, height: 600 });
    await page.goto(`/sessions/${SESSION}`);

    const badge = page.locator(".model-badge");
    await expect(badge).toContainText("claude-sonnet-4-20250514", {
      timeout: 5_000,
    });
    await assertRenderLint(page, width);
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
