import { expect, test } from "@playwright/test";

// 5,500 messages alternating user and assistant: the app loads only the
// newest page, so the first prompts start out unloaded. User messages sit
// at even ordinals and read "User message <ordinal> of 5500."
const SESSION_ID = "test-session-xlarge-5500";

test("jumps from a prompt search to a prompt that is not loaded yet", async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem("agentsview-message-layout", "default"));
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(`/sessions/${SESSION_ID}`);
  await expect(
    page.locator(`.message-list-scroll[data-session-id="${SESSION_ID}"][data-loaded="true"]`),
  ).toBeVisible();
  const target = page.locator(".message-list-scroll .message", {
    hasText: "User message 1000 of 5500.",
  });
  await expect(target).toHaveCount(0);

  await page.keyboard.press("t");
  const search = page.getByRole("combobox", { name: "Search prompts" });
  await expect(search).toBeFocused();
  await expect(page.locator(".picker-status")).toHaveText(
    "Showing the newest 100 of 2,750. Type to narrow.",
  );
  await search.fill("1000");
  await expect(page.getByRole("option")).toHaveText([/User message 1000 of 5500\..*#501/]);
  await page.keyboard.press("Enter");

  await expect(target).toBeInViewport();
  // The jump lands the prompt at the top of the transcript.
  const offset = await target.evaluate((el) => {
    const list = el.closest(".message-list-scroll")!.getBoundingClientRect();
    return el.getBoundingClientRect().top - list.top;
  });
  expect(offset).toBeGreaterThanOrEqual(0);
  expect(offset).toBeLessThan(40);
});
