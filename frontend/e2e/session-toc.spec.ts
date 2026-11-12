import { expect, test } from "@playwright/test";

// 5,500 messages alternating user and assistant: the app loads only the
// newest page, so the first prompts start out unloaded.
const SESSION_ID = "test-session-xlarge-5500";
const PROMPT_COUNT = 2750;

test.describe("session prompt outline", () => {
  test.beforeEach(async ({ page }) => {
    await page.addInitScript(() => localStorage.setItem("agentsview-message-layout", "default"));
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto(`/sessions/${SESSION_ID}`);
    await expect(
      page.locator(`.message-list-scroll[data-session-id="${SESSION_ID}"][data-loaded="true"]`),
    ).toBeVisible();
  });

  test("jumps from the keyboard to a prompt that is not loaded yet", async ({ page }) => {
    const scroller = page.locator(".message-list-scroll");
    const target = scroller.locator(".message", { hasText: "User message 2 of 5500." });
    await expect(target).toHaveCount(0);

    await page.keyboard.press("t");
    const toc = page.getByRole("navigation", { name: "Prompts" });
    await expect(toc).toBeVisible();
    await expect(page.getByRole("button", { name: "Prompt outline", exact: true })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    // The count appears once every prompt older than the loaded window is read.
    await expect(toc.locator(".toc-count")).toHaveText(
      `${PROMPT_COUNT.toLocaleString("en-US")} prompts`,
    );
    await expect(toc.locator(".toc-entry:focus")).toHaveCount(1);

    await page.keyboard.press("Home");
    await page.keyboard.press("ArrowDown");
    const entry = toc.locator('.toc-entry[data-ordinal="2"]');
    await expect(entry).toBeFocused();
    await expect(entry).toContainText("User message 2 of 5500.");
    await page.keyboard.press("Enter");

    await expect(target).toBeInViewport();
    await expect(entry).toHaveAttribute("aria-current", "location");
    // The jump lands the prompt at the top of the transcript, not just anywhere on screen.
    const offset = await target.evaluate((el) => {
      const list = el.closest(".message-list-scroll")!.getBoundingClientRect();
      return el.getBoundingClientRect().top - list.top;
    });
    expect(offset).toBeGreaterThanOrEqual(0);
    expect(offset).toBeLessThan(40);

    await page.keyboard.press("Escape");
    await expect(toc).toBeHidden();
    await expect(page.getByRole("button", { name: "Prompt outline", exact: true })).toBeFocused();
  });

  test("follows the reader, stays open across reloads, and jumps on click", async ({ page }) => {
    await page.getByRole("button", { name: "Prompt outline", exact: true }).click();
    const toc = page.getByRole("navigation", { name: "Prompts" });
    await expect(toc.locator(".toc-count")).toHaveText(
      `${PROMPT_COUNT.toLocaleString("en-US")} prompts`,
    );

    await page.reload();
    await expect(toc).toBeVisible();
    // The transcript opens at its newest page, so the current prompt sits
    // far down a list that earlier prompts fill in after it renders.
    await expect(toc.locator(".toc-count")).toHaveText(
      `${PROMPT_COUNT.toLocaleString("en-US")} prompts`,
    );
    const opened = toc.locator('[aria-current="location"]');
    await expect(opened).toHaveAttribute("data-ordinal", "4500");
    await expect(opened).toBeInViewport();

    const entry = toc.locator('.toc-entry[data-ordinal="1000"]');
    await entry.scrollIntoViewIfNeeded();
    await entry.click();
    const target = page.locator(".message-list-scroll .message", {
      hasText: "User message 1000 of 5500.",
    });
    await expect(target).toBeInViewport();
    await expect(entry).toHaveAttribute("aria-current", "location");

    // Scrolling the transcript by hand moves the mark to the turn on screen.
    await page.locator(".message-list-scroll").hover();
    await page.mouse.wheel(0, 900);
    await expect(toc.locator('[aria-current="location"]')).not.toHaveAttribute(
      "data-ordinal",
      "1000",
    );
    const marked = Number(
      await toc.locator('[aria-current="location"]').getAttribute("data-ordinal"),
    );
    expect(marked).toBeGreaterThan(1000);
    await expect(
      page.locator(".message-list-scroll .message", { hasText: `User message ${marked} of 5500.` }),
    ).toBeAttached();
  });
});
