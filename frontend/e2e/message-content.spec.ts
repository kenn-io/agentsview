import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { test, expect, type Locator, type Page } from "@playwright/test";

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

const TOOL_BLOCK_PATH =
  "/workspace/packages/agentsview/frontend/src/lib/components/content/ToolBlock.svelte";
const TOOL_VIEWPORTS = [1280, 768] as const;
const TOOL_THEMES = ["light", "dark"] as const;

function getSessionItem(page: Page, project: string, count: number) {
  return page
    .locator(LOC.sessionItem)
    .filter({
      has: page.locator(`${LOC.sessionProject}:text-is("${project}")`),
    })
    .filter({
      has: page.locator(`${LOC.sessionCount}:text-is("${count}")`),
    });
}

async function selectSession(page: Page, project: string, count: number): Promise<string> {
  const item = getSessionItem(page, project, count);
  const sessionId = await item.getAttribute("data-session-id");
  expect(sessionId).toBeTruthy();
  await expect(item).toBeVisible();
  await page.goto(`/sessions/${encodeURIComponent(sessionId!)}`);
  await expect(item).toHaveClass(/active/);
  return sessionId!;
}

async function expectSessionLoaded(page: Page, sessionId: string, expectedRows?: number) {
  const messageList = page.locator(LOC.listScroll);
  await expect(messageList).toHaveAttribute("data-session-id", sessionId);
  await expect(messageList).toHaveAttribute("data-messages-session-id", sessionId);
  await expect(messageList).toHaveAttribute("data-loaded", "true");

  if (expectedRows !== undefined) {
    await expect(page.locator(LOC.row)).toHaveCount(expectedRows);
  } else {
    await expect(page.locator(LOC.row).first()).toBeVisible();
  }
}

test.describe("Mixed content rendering", () => {
  test.describe.configure({ timeout: COLD_WEBKIT_TEST_TIMEOUT_MS });

  test.beforeEach(async ({ page }) => {
    await page.goto("/sessions");
    await expect(page.locator(LOC.sessionItem).first()).toBeVisible({ timeout: 5_000 });
  });

  test("tool group renders for consecutive tool-only messages", async ({ page }) => {
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

  test("tool block expands on click and text is selectable", async ({ page }) => {
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

  test("tool output raw/formatted selection preserves the current output", async ({ page }) => {
    const { project, count, displayRows } = BETA_7;
    for (const theme of TOOL_THEMES) {
      await page.evaluate((value) => localStorage.setItem("theme", value), theme);
      for (const width of TOOL_VIEWPORTS) {
        await page.setViewportSize({ width, height: 900 });
        await page.reload();
        await expect(page.locator(LOC.sessionItem).first()).toBeVisible({ timeout: 5_000 });
        const sid = await selectSession(page, project, count);
        await expectSessionLoaded(page, sid, displayRows);
        const pathTool = page
          .locator(".tool-block")
          .filter({
            has: page.locator(`.tool-preview[title="${TOOL_BLOCK_PATH}"]`),
          })
          .first();
        const pathPreview = pathTool.locator(".tool-preview");
        await expect(pathPreview).toHaveAttribute("title", TOOL_BLOCK_PATH);
        await expect(pathPreview.locator(".kit-sr-only")).toHaveText(TOOL_BLOCK_PATH);
        await pathTool.locator(".tool-header").click();
        const outputHeader = pathTool.locator(".output-header");
        await expect(outputHeader).toBeVisible();
        await expect(pathTool.locator(".output-mode")).toHaveCount(0);
        await outputHeader.click();
        const mode = pathTool.getByRole("radiogroup", { name: "Output format" });
        await expect(mode).toBeVisible();
        await mode.getByRole("radio", { name: "Formatted" }).click();
        const formattedOutput = pathTool.locator(".formatted-output");
        await expect(formattedOutput).toBeVisible();
        await expect(formattedOutput).toContainText("safe");
        await expect(formattedOutput.locator("script")).toHaveCount(0);
        await mode.getByRole("radio", { name: "Raw" }).click();
        await expect(pathTool.locator(".formatted-output")).toHaveCount(0);
        await expect(pathTool.locator(".output-content")).toContainText(
          '<script>alert("xss")</script>',
        );
      }
    }
  });

  test("text selection does not collapse tool block", async ({ page }) => {
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
    await page.evaluate(() => window.getSelection()?.removeAllRanges());
    await toolHeader.click();
    await expect(toolContent).not.toBeVisible();
  });

  test("thinking block is collapsed by default", async ({ page }) => {
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
    await expect(thinkingContent).toContainText("Let me analyze...");
  });

  test("thinking+text message shows response text", async ({ page }) => {
    const { project, count, displayRows } = BETA_7;
    const sid = await selectSession(page, project, count);
    await expectSessionLoaded(page, sid, displayRows);

    // The response text after thinking should be visible
    await expect(
      page.locator(LOC.row).filter({
        hasText: "visible response after thinking",
      }),
    ).toBeVisible();
  });

  test("response text remains after toggling thinking off", async ({ page }) => {
    const { project, count, displayRows } = BETA_7;
    const sid = await selectSession(page, project, count);
    await expectSessionLoaded(page, sid, displayRows);

    // Open block filter dropdown and toggle thinking off
    await page.locator('button[aria-label="Filter block types"]').click();
    await page.locator(".block-filter-item").filter({ hasText: "Thinking blocks" }).click();

    // Thinking blocks should be hidden
    const thinkingBlocks = page.locator(".thinking-block");
    await expect(thinkingBlocks).toHaveCount(0);

    // Response text should still be visible
    await expect(
      page.locator(LOC.row).filter({
        hasText: "visible response after thinking",
      }),
    ).toBeVisible();
  });
});

test.describe("retained tool images", () => {
  test.describe.configure({ timeout: 60_000 });

  test("renders retained, image-only, migrated, and fallback tool images", async ({
    page,
  }, testInfo) => {
    const fixturePath = fileURLToPath(
      new URL("../src/lib/utils/__fixtures__/retained-tool-image-1735.json", import.meta.url),
    );
    const retainedResult = await readFile(fixturePath, "utf8");
    const retainedBlocks = JSON.parse(retainedResult) as Array<{ image_url?: string }>;
    const retainedImageURL = retainedBlocks[1]?.image_url;
    const retainedBytes = Buffer.byteLength(retainedResult, "utf8");
    const retainedHash = createHash("sha256").update(retainedResult).digest("hex");
    const smallPNG =
      "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=";
    const migratedResult = JSON.stringify([
      { type: "input_text", text: "Before" },
      { type: "agentsview_image", version: 1, text: "![first](asset://first)" },
      { type: "agentsview_image", version: 1, text: "![second](asset://nested/second)" },
      { type: "text", text: "After" },
    ]);
    const unsupportedResult = retainedResult.replace("image/png", "image/svg+xml");
    const sessionId = "retained-tool-image-1735";
    const now = "2026-09-11T12:00:00Z";
    const session = {
      id: sessionId,
      parent_session_id: null,
      relationship_type: null,
      project: "retained-images",
      machine: "test-machine",
      agent: "test-agent",
      first_message: "Retained tool image test",
      display_name: "Retained tool image test",
      started_at: now,
      ended_at: now,
      message_count: 3,
      user_message_count: 3,
      created_at: now,
      file_path: "/tmp/retained-tool-image-1735.json",
      termination_status: null,
      is_automated: false,
      is_teammate: false,
    };
    const toolCalls = [
      {
        category: "Other",
        tool_name: "retained_tool_image",
        result_content: retainedResult,
        result_content_length: retainedBytes,
      },
      {
        category: "Other",
        tool_name: "image_only_tool_image",
        result_content: JSON.stringify([{ type: "input_image", image_url: smallPNG }]),
        result_content_length: smallPNG.length,
      },
      {
        category: "Other",
        tool_name: "migrated_tool_image",
        result_content: migratedResult,
        result_content_length: migratedResult.length,
      },
      {
        category: "Other",
        tool_name: "unsupported_tool_image",
        result_content: unsupportedResult,
        result_content_length: unsupportedResult.length,
      },
    ];
    const messages = toolCalls.map((toolCall, index) => ({
      id: index + 1,
      session_id: sessionId,
      ordinal: index,
      role: "assistant",
      content: "",
      timestamp: now,
      has_thinking: false,
      thinking_text: "",
      has_tool_use: true,
      content_length: 0,
      model: "",
      token_usage: null,
      context_tokens: 0,
      output_tokens: 0,
      has_context_tokens: false,
      has_output_tokens: false,
      tool_calls: [toolCall],
      is_system: false,
    }));
    const assetBytes = Buffer.from(smallPNG.split(",", 2)[1]!, "base64");
    await page.route("**/api/v1/**", (route) => route.abort());
    await page.route("**/api/v1/sessions**", async (route) => {
      const pathname = new URL(route.request().url()).pathname;
      if (pathname.endsWith("/sidebar-index") || pathname.endsWith("/sessions")) {
        await route.fulfill({
          json: {
            sessions: [
              {
                ...session,
                message_count: 3,
                user_message_count: 3,
              },
            ],
            total: 1,
            next_cursor: null,
          },
        });
        return;
      }
      if (pathname.endsWith(`/sessions/${sessionId}`)) {
        await route.fulfill({ json: session });
        return;
      }
      if (pathname.includes(`/sessions/${sessionId}/messages`)) {
        await route.fulfill({ json: { messages, count: messages.length } });
        return;
      }
      await route.abort();
    });
    await page.addInitScript(() => {
      localStorage.setItem("agentsview-auth-token", "test-asset-token");
    });
    for (const assetPath of ["first", "nested%2Fsecond"]) {
      await page.route(`**/api/v1/assets/${assetPath}`, async (route) => {
        expect(route.request().headers()["authorization"]).toBe("Bearer test-asset-token");
        await route.fulfill({ body: assetBytes, contentType: "image/png" });
      });
    }

    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto(`/sessions/${sessionId}`);
    await expect(page.locator(".tool-block")).toHaveCount(4);

    const retainedBlock = page
      .locator(".tool-block")
      .filter({ hasText: "retained_tool_image" })
      .first();
    const imageOnlyBlock = page
      .locator(".tool-block")
      .filter({ hasText: "image_only_tool_image" })
      .first();
    const migratedBlock = page
      .locator(".tool-block")
      .filter({ hasText: "migrated_tool_image" })
      .first();
    const unsupportedBlock = page
      .locator(".tool-block")
      .filter({ hasText: "unsupported_tool_image" })
      .first();
    await expect(retainedBlock).toBeVisible();
    await expect(imageOnlyBlock).toBeVisible();
    await expect(migratedBlock).toBeVisible();
    await expect(unsupportedBlock).toBeVisible();

    async function openFormatted(block: Locator) {
      await block.locator(".tool-header").click();
      await block.locator(".output-header").click();
      const mode = block.getByRole("radiogroup", { name: "Output format" });
      await expect(mode).toBeVisible();
      await mode.getByRole("radio", { name: "Formatted" }).click();
      const formatted = block.locator(".formatted-output");
      await expect(formatted).toBeVisible();
      return { formatted, mode };
    }

    await retainedBlock.locator(".tool-header").click();
    await retainedBlock.locator(".output-header").click();
    const retainedRaw = retainedBlock.locator(".output-content");
    await expect(retainedRaw).toBeVisible();
    expect(await retainedRaw.textContent()).toBe(retainedResult.replace(/\r\n/g, "\n"));
    const retainedMode = retainedBlock.getByRole("radiogroup", { name: "Output format" });
    await retainedMode.getByRole("radio", { name: "Formatted" }).click();
    const retainedFormatted = retainedBlock.locator(".formatted-output");
    await expect(retainedFormatted).toBeVisible();
    await retainedFormatted.screenshot({ path: testInfo.outputPath("agentsview-1735-before.png") });
    const retainedImage = retainedFormatted.locator("img");
    await expect(retainedImage).toHaveCount(1);
    await expect(retainedImage).toHaveAttribute("src", retainedImageURL!);
    await expect
      .poll(() =>
        retainedImage.evaluate((img: HTMLImageElement) => ({
          complete: img.complete,
          naturalWidth: img.naturalWidth,
          naturalHeight: img.naturalHeight,
        })),
      )
      .toEqual({ complete: true, naturalWidth: 600, naturalHeight: 600 });
    const retainedText = await retainedFormatted.textContent();
    expect(retainedText).not.toContain("input_image");
    expect(retainedText).not.toContain(retainedImageURL!);

    const widths = [1280, 768, 400] as const;
    for (const width of widths) {
      await page.setViewportSize({ width, height: 900 });
      const measurement = await retainedFormatted.evaluate((element) => {
        const image = element.querySelector("img")!;
        const imageRect = image.getBoundingClientRect();
        return {
          clientWidth: element.clientWidth,
          scrollWidth: element.scrollWidth,
          imageWidth: imageRect.width,
          imageHeight: imageRect.height,
        };
      });
      expect(measurement.scrollWidth).toBeLessThanOrEqual(measurement.clientWidth + 2);
      expect(measurement.imageWidth).toBeLessThanOrEqual(measurement.clientWidth + 2);
      console.log(`retained layout width=${width}px ${JSON.stringify(measurement)}`);
      if (width === 1280) {
        await retainedFormatted.screenshot({
          path: testInfo.outputPath("agentsview-1735-after.png"),
        });
      } else {
        await retainedFormatted.screenshot({
          path: testInfo.outputPath(
            width === 768 ? "agentsview-1735-after-768.png" : "agentsview-1735-after-400.png",
          ),
        });
      }
    }

    await retainedMode.getByRole("radio", { name: "Raw" }).click();
    await expect(retainedBlock.locator(".output-content")).toHaveText(
      retainedResult.replace(/\r\n/g, "\n"),
    );
    await expect(retainedBlock.locator(".output-content img")).toHaveCount(0);

    const imageOnly = await openFormatted(imageOnlyBlock);
    const imageOnlyImage = imageOnly.formatted.locator("img");
    await expect(imageOnlyImage).toHaveCount(1);
    await expect(imageOnlyImage).toHaveAttribute("src", smallPNG);
    await expect
      .poll(() =>
        imageOnlyImage.evaluate((img: HTMLImageElement) => ({
          complete: img.complete,
          naturalWidth: img.naturalWidth,
          naturalHeight: img.naturalHeight,
        })),
      )
      .toEqual({ complete: true, naturalWidth: 1, naturalHeight: 1 });
    expect(await imageOnly.formatted.textContent()).not.toContain("input_image");

    const migrated = await openFormatted(migratedBlock);
    const migratedImages = migrated.formatted.locator("img");
    await expect(migratedImages).toHaveCount(2);
    await expect(migratedImages.nth(0)).toHaveAttribute("src", /^blob:/);
    await expect(migratedImages.nth(1)).toHaveAttribute("src", /^blob:/);
    await expect
      .poll(() =>
        migratedImages.evaluateAll((images) =>
          images.map((image) => ({
            complete: (image as HTMLImageElement).complete,
            naturalWidth: (image as HTMLImageElement).naturalWidth,
            naturalHeight: (image as HTMLImageElement).naturalHeight,
          })),
        ),
      )
      .toEqual([
        { complete: true, naturalWidth: 1, naturalHeight: 1 },
        { complete: true, naturalWidth: 1, naturalHeight: 1 },
      ]);
    expect(await migrated.formatted.textContent()).toContain("Before");
    expect(await migrated.formatted.textContent()).toContain("After");
    expect(await migrated.formatted.textContent()).not.toContain("asset://");

    const unsupported = await openFormatted(unsupportedBlock);
    await expect(unsupported.formatted.locator("img")).toHaveCount(0);
    expect(await unsupported.formatted.textContent()).toContain("input_image");
    const unsupportedLayout = await unsupported.formatted.evaluate((element) => ({
      clientWidth: element.clientWidth,
      scrollWidth: element.scrollWidth,
    }));
    expect(unsupportedLayout.scrollWidth).toBeLessThanOrEqual(unsupportedLayout.clientWidth + 2);
    console.log(`unsupported layout ${JSON.stringify(unsupportedLayout)}`);
    expect(retainedBytes).toBe(1_441_138);
    expect(retainedHash).toBe("0cd12cbc57b1b4ea2cadca8f96fb7b48bfeb5ac65db6d6f8cecee074a4687dac");
  });
});
