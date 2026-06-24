import { test, expect, type Page } from "@playwright/test";

const LOC = {
  sessionItem: ".session-item",
  sessionProject: ".session-project",
  sessionCount: ".session-count",
  listScroll: ".message-list-scroll",
  row: ".virtual-row",
} as const;

const COLD_WEBKIT_TEST_TIMEOUT_MS = 30_000;

const BETA_7 = {
  project: "project-beta",
  count: 3, // user_message_count shown in sidebar
  displayRows: 6,
};

function getSessionItem(
  page: Page,
  project: string,
  count: number,
) {
  return page
    .locator(LOC.sessionItem)
    .filter({
      has: page.locator(
        `${LOC.sessionProject}:text-is("${project}")`,
      ),
    })
    .filter({
      has: page.locator(
        `${LOC.sessionCount}:text-is("${count}")`,
      ),
    });
}

async function selectSession(
  page: Page,
  project: string,
  count: number,
): Promise<string> {
  const item = getSessionItem(page, project, count);
  const sessionId = await item.getAttribute("data-session-id");
  expect(sessionId).toBeTruthy();
  await expect(item).toBeVisible();
  await item.click({ force: true });
  await expect(item).toHaveClass(/active/);
  return sessionId!;
}

async function expectSessionLoaded(
  page: Page,
  sessionId: string,
  expectedRows?: number,
) {
  const messageList = page.locator(LOC.listScroll);
  await expect(messageList).toHaveAttribute(
    "data-session-id",
    sessionId,
  );
  await expect(messageList).toHaveAttribute(
    "data-messages-session-id",
    sessionId,
  );
  await expect(messageList).toHaveAttribute(
    "data-loaded",
    "true",
  );

  if (expectedRows !== undefined) {
    await expect(page.locator(LOC.row)).toHaveCount(expectedRows);
  } else {
    await expect(
      page.locator(LOC.row).first(),
    ).toBeVisible();
  }
}

test.describe("Mixed content rendering", () => {
  test.describe.configure({ timeout: COLD_WEBKIT_TEST_TIMEOUT_MS });

  test.beforeEach(async ({ page }) => {
    await page.goto("/");
    await expect(
      page.locator(LOC.sessionItem).first(),
    ).toBeVisible({ timeout: 5_000 });
  });

  test("tool group renders for consecutive tool-only messages", async ({
    page,
  }) => {
    const { project, count, displayRows } = BETA_7;
    const sid = await selectSession(page, project, count);
    await expectSessionLoaded(page, sid, displayRows);

    const toolGroup = page.locator(".tool-group");
    await expect(toolGroup).toBeVisible();
    await expect(toolGroup).toContainText(/tool calls?/i);

    const toolGroupBody = page.locator(".tool-group-body");
    await expect(toolGroupBody).toBeVisible();

    // Should contain exactly 2 tool blocks inside the group
    // (Indices 3 and 4 in the fixture are tool calls)
    const toolBlocks = toolGroupBody.locator(".tool-block");
    await expect(toolBlocks).toHaveCount(2);
  });

  test("tool block expands on click and text is selectable", async ({
    page,
  }) => {
    const { project, count, displayRows } = BETA_7;
    const sid = await selectSession(page, project, count);
    await expectSessionLoaded(page, sid, displayRows);

    const toolBlock = page.locator(".tool-block").first();
    await expect(toolBlock).toBeVisible();

    // Tool content should be hidden (collapsed by default)
    const toolContent = toolBlock.locator(".tool-content");
    await expect(toolContent).not.toBeVisible();

    // Click the header to expand
    const toolHeader = toolBlock.locator(".tool-header");
    await toolHeader.click();

    // Content should now be visible
    await expect(toolContent).toBeVisible();

    // Verify text is selectable inside the tool content
    const isSelectable = await toolContent.evaluate((el) => {
      const style = window.getComputedStyle(el);
      return style.userSelect !== "none";
    });
    expect(isSelectable).toBe(true);

    // Verify the tool header button allows text selection
    const headerSelectable = await toolHeader.evaluate((el) => {
      const style = window.getComputedStyle(el);
      return style.userSelect !== "none";
    });
    expect(headerSelectable).toBe(true);
  });

  test("text selection does not collapse tool block", async ({
    page,
  }) => {
    const { project, count, displayRows } = BETA_7;
    const sid = await selectSession(page, project, count);
    await expectSessionLoaded(page, sid, displayRows);

    // Expand the tool block first
    const toolBlock = page.locator(".tool-block").first();
    const toolHeader = toolBlock.locator(".tool-header");
    await toolHeader.click();

    const toolContent = toolBlock.locator(".tool-content");
    await expect(toolContent).toBeVisible();

    // Simulate a text selection then click the header
    // The block should remain expanded because there's a selection
    await toolContent.evaluate((el) => {
      const range = document.createRange();
      range.selectNodeContents(el);
      const sel = window.getSelection()!;
      sel.removeAllRanges();
      sel.addRange(range);
    });
    await toolHeader.click();

    // Tool content should still be visible (click was suppressed)
    await expect(toolContent).toBeVisible();

    // Clear selection and click again - now it should collapse
    await page.evaluate(() =>
      window.getSelection()?.removeAllRanges(),
    );
    await toolHeader.click();
    await expect(toolContent).not.toBeVisible();
  });

  test("thinking block is collapsed by default", async ({
    page,
  }) => {
    const { project, count, displayRows } = BETA_7;
    const sid = await selectSession(page, project, count);
    await expectSessionLoaded(page, sid, displayRows);

    const thinkingBlock = page.locator(".thinking-block").first();
    await expect(thinkingBlock).toBeVisible();

    // Content should be hidden (collapsed by default)
    const thinkingContent = thinkingBlock.locator(".thinking-content");
    await expect(thinkingContent).not.toBeVisible();

    // Click to expand
    const thinkingHeader = thinkingBlock.locator(".thinking-header");
    await thinkingHeader.click();

    // Content should now be visible
    await expect(thinkingContent).toBeVisible();
    await expect(thinkingContent).toContainText(
      "Let me analyze...",
    );
  });

  test("thinking+text message shows response text", async ({
    page,
  }) => {
    const { project, count, displayRows } = BETA_7;
    const sid = await selectSession(page, project, count);
    await expectSessionLoaded(page, sid, displayRows);

    // The response text after thinking should be visible
    await expect(
      page
        .locator(LOC.row)
        .filter({
          hasText: "visible response after thinking",
        }),
    ).toBeVisible();
  });

  test("response text remains after toggling thinking off", async ({
    page,
  }) => {
    const { project, count, displayRows } = BETA_7;
    const sid = await selectSession(page, project, count);
    await expectSessionLoaded(page, sid, displayRows);

    // Open block filter dropdown and toggle thinking off
    await page
      .locator('button[aria-label="Filter block types"]')
      .click();
    await page
      .locator(".block-filter-item")
      .filter({ hasText: "Thinking blocks" })
      .click();

    // Thinking blocks should be hidden
    const thinkingBlocks = page.locator(".thinking-block");
    await expect(thinkingBlocks).toHaveCount(0);

    // Response text should still be visible
    await expect(
      page
        .locator(LOC.row)
        .filter({
          hasText: "visible response after thinking",
        }),
    ).toBeVisible();
  });
});
