import { expect, test } from "@playwright/test";

// 5,500 messages alternating user and assistant: the app loads only the
// newest page, so the first prompts start out unloaded. User messages sit
// at even ordinals and read "User message <ordinal> of 5500."
const SESSION_ID = "test-session-xlarge-5500";

test.describe("session prompt picker", () => {
  test.beforeEach(async ({ page }) => {
    await page.addInitScript(() => localStorage.setItem("agentsview-message-layout", "default"));
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto(`/sessions/${SESSION_ID}`);
    await expect(
      page.locator(`.message-list-scroll[data-session-id="${SESSION_ID}"][data-loaded="true"]`),
    ).toBeVisible();
  });

  test("searches every prompt from the keyboard and jumps to one not loaded yet", async ({
    page,
  }) => {
    const target = page.locator(".message-list-scroll .message", {
      hasText: "User message 1000 of 5500.",
    });
    await expect(target).toHaveCount(0);

    await page.keyboard.press("t");
    const search = page.getByRole("combobox", { name: "Search prompts" });
    await expect(search).toBeFocused();
    await search.fill("1000");
    // The count covers every prompt once the ones before the loaded page are read.
    await expect(page.locator(".picker-status")).toHaveText("1 of 2,750 prompts");
    await expect(page.getByRole("option")).toHaveText([/User message 1000 of 5500\..*#501/]);
    await page.keyboard.press("Enter");

    await expect(target).toBeInViewport();
    // The jump lands the prompt at the top of the transcript, not just anywhere on screen.
    const offset = await target.evaluate((el) => {
      const list = el.closest(".message-list-scroll")!.getBoundingClientRect();
      return el.getBoundingClientRect().top - list.top;
    });
    expect(offset).toBeGreaterThanOrEqual(0);
    expect(offset).toBeLessThan(40);
    await expect(page.getByRole("button", { name: "Jump to prompt" })).toBeFocused();
  });

  test("opens on the prompt being read", async ({ page }) => {
    const button = page.getByRole("button", { name: "Jump to prompt" });
    await button.click();
    // The transcript opens at the top of its newest page, prompt 2,251, so
    // the picker opens on the prompts around it.
    await expect(page.locator(".picker-status")).toHaveText(
      "Prompts 2,201–2,300 of 2,750. Type to search them all.",
    );
    await expect(page.getByRole("option", { selected: true })).toContainText(
      "User message 4500 of 5500.",
    );

    await page.getByRole("combobox", { name: "Search prompts" }).fill("1000");
    await page.getByRole("option", { name: /User message 1000 of 5500\./ }).click();
    await expect(
      page.locator(".message-list-scroll .message", { hasText: "User message 1000 of 5500." }),
    ).toBeInViewport();

    await button.click();
    await expect(page.locator(".picker-status")).toHaveText(
      "Prompts 451–550 of 2,750. Type to search them all.",
    );
    await expect(page.getByRole("option", { selected: true })).toContainText(
      "User message 1000 of 5500.",
    );
    await page.keyboard.press("Escape");
    await expect(page.getByRole("combobox", { name: "Search prompts" })).toHaveCount(0);
    await expect(button).toBeFocused();
  });
});
