import { expect, test, type Page } from "@playwright/test";
import { HOOK_SESSION_ID, installHooksFixture } from "./hooks.fixture";

async function openHookTranscript(page: Page) {
  await installHooksFixture(page);
  await page.goto(`/sessions/${HOOK_SESSION_ID}`);
  await expect(page.locator('.message-list-scroll[data-loaded="true"]')).toBeVisible();
  await expect(page.getByText("Final reply", { exact: true })).toBeVisible();
}

function hookCard(page: Page) {
  return page
    .locator(".system-boundary")
    .filter({ has: page.locator(".label", { hasText: "Hook output" }) });
}

async function openBlockMenu(page: Page) {
  await page.locator(".transcript-strip .filter-wrap > button").click();
  return page.locator(".block-filter-dropdown");
}

async function expectFollowingReplyBelowHook(page: Page) {
  await expect
    .poll(async () => {
      const card = await hookCard(page).boundingBox();
      const reply = await page.getByText("Final reply", { exact: true }).boundingBox();
      return !!card && !!reply && reply.y >= card.y + card.height;
    })
    .toBe(true);
}

test("hook output is collapsed and keyboard expandable", async ({ page }) => {
  await openHookTranscript(page);
  const card = hookCard(page);
  await expect(card).toBeVisible();
  await expect(card.locator("details")).not.toHaveAttribute("open");
  await card.locator("summary").focus();
  await page.keyboard.press("Enter");
  await expect(card.locator("pre")).toBeVisible();
  await expectFollowingReplyBelowHook(page);
});

test("Hooks and System boundaries have independent persisted filters", async ({ page }) => {
  await openHookTranscript(page);
  const menu = await openBlockMenu(page);
  await menu.getByRole("button", { name: "System boundaries", exact: true }).click();
  await expect(hookCard(page)).toBeVisible();
  await expect(
    page.locator(".system-boundary .label", { hasText: "Session continuation" }),
  ).toHaveCount(0);
  await menu.getByRole("button", { name: "Hooks", exact: true }).click();
  await expect(hookCard(page)).toHaveCount(0);
  await page.reload();
  await expect(page.getByText("Final reply", { exact: true })).toBeVisible();
  await expect(hookCard(page)).toHaveCount(0);
  const restored = await openBlockMenu(page);
  await expect(restored.getByRole("button", { name: "Hooks", exact: true })).not.toHaveClass(
    /\bactive\b/,
  );
  await restored.locator(".block-filter-reset").click();
  await expect(hookCard(page)).toBeVisible();
});

for (const width of [1440, 1280, 768, 390]) {
  test(`hook card remains contained at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await openHookTranscript(page);
    const card = hookCard(page);
    await expect(card).toBeVisible();
    await card.locator("summary").click();
    const bounds = await card.boundingBox();
    expect(bounds).not.toBeNull();
    expect(bounds!.x).toBeGreaterThanOrEqual(0);
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width);
    await expect(card.locator("pre")).toBeVisible();
    await expectFollowingReplyBelowHook(page);
  });
}
