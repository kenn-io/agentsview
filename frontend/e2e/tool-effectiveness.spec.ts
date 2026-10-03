import { test, expect, type Page } from "@playwright/test";

const SESSION_ID = "test-session-tool-sequences";

const report = {
  id: 77,
  type: "tool_effectiveness",
  date_from: "2026-04-26",
  date_to: "2026-04-26",
  project: "tool-sequences-test",
  agent: "claude",
  model: "test-model",
  prompt: null,
  content: "## Model assessment\n\n- saved text",
  schema_version: "tool_effectiveness.v1",
  structured_json: JSON.stringify({
    session_id: SESSION_ID,
    call_count: 3,
    conclusions: [
      {
        assessment: "did_not_help",
        text: "The second Grep repeated an empty search.",
        ordinals: [1, 2],
        calls: [{ ordinal: 2, call_index: 0 }],
      },
      {
        assessment: "helped",
        text: "Reading the config file found the setting.",
        ordinals: [3],
        calls: [],
      },
    ],
    omissions: [
      { reason: "previews", count: 1 },
      { reason: "budget", ordinal: 3, call_index: 0, tool_name: "Read", field: "result", kept_bytes: 12, original_bytes: 16 },
    ],
    cited_calls: [
      { ordinal: 1, call_index: 0, tool_name: "Grep", input_preview: '{"pattern":"config"}', outcome: "empty", result_bytes: 16, message_calls: 1 },
      { ordinal: 2, call_index: 0, tool_name: "Grep", input_preview: '{"pattern":"config"}', outcome: "empty", result_bytes: 16, message_calls: 1 },
      { ordinal: 3, call_index: 0, tool_name: "mcp__onemcp__context7_1mcp_resolve_library_id", input_preview: '{"file_path":"app/config.json"}', outcome: "content", result_bytes: 16, message_calls: 1 },
    ],
  }),
  created_at: "2026-04-26T12:00:00Z",
};

async function openReport(page: Page, width: number) {
  await page.setViewportSize({ width, height: 900 });
  await page.goto(`/recall?tab=generated&insight=${report.id}`, { waitUntil: "domcontentloaded" });
  const detail = page.locator(".generated-detail");
  await expect(detail.locator(".tool-sequences-panel")).toBeVisible({ timeout: 10_000 });
  return detail;
}

test.beforeEach(async ({ page }) => {
  await page.route("**/api/v1/insights", (route) =>
    route.fulfill({ json: { insights: [report] } }),
  );
});

test("saved tool-effectiveness report keeps conclusions, sequences, and omissions at each width", async ({
  page,
}, testInfo) => {
  for (const width of [1280, 768, 400]) {
    const detail = await openReport(page, width);
    await expect(detail.getByRole("heading", { name: "Model assessment" })).toBeVisible();
    await expect(detail.getByRole("heading", { name: "Observed tool sequences" })).toBeVisible();
    await expect(detail).toContainText("The second Grep repeated an empty search.");
    await expect(detail).toContainText("1 message shown as an 800-character preview");
    await expect(detail).toContainText("3 of 3 calls cited");
    await expect(detail.locator("[data-testid=tool-effectiveness-session-changed]")).toHaveCount(0);
    await expect(detail.locator(".citations .citation")).toHaveText([
      /Grep\s*config\s*· Message 1/,
      /Grep\s*config\s*· Message 2/,
      /mcp__onemcp__context7_1mcp_resolve_library_id\s*app\/config\.json\s*· Message 3/,
    ]);

    const evidence = detail.locator(".conclusion").nth(1);
    const citationInput = await evidence.locator(".citation .citation-input").boundingBox();
    expect(citationInput!.width).toBeGreaterThan(20);
    await evidence.locator(".citation").click();
    const row = evidence.locator(".call");
    await expect(row).toContainText("model saw 12 bytes");
    const [input, result, jump] = await Promise.all(
      [".input", ".res", ".jump"].map((s) => row.locator(s).boundingBox()),
    );
    // The long tool name must not squeeze the input preview out.
    expect(input!.width).toBeGreaterThan(40);
    const overlaps = (a: typeof result, b: typeof result) =>
      a!.x < b!.x + b!.width && b!.x < a!.x + a!.width && a!.y < b!.y + b!.height && b!.y < a!.y + a!.height;
    expect(overlaps(result, jump)).toBe(false);
    expect(overlaps(input, jump)).toBe(false);

    const overflow = await detail.evaluate((element) => element.scrollWidth - element.clientWidth);
    expect(overflow).toBeLessThanOrEqual(1);
    await page.screenshot({
      path: testInfo.outputPath(`tool-effectiveness-${width}.png`),
      fullPage: true,
    });
  }
});

test("cited calls and sequence calls open the transcript at the cited message", async ({
  page,
}) => {
  let detail = await openReport(page, 1280);
  const conclusion = detail.locator(".conclusion").nth(1);
  await conclusion.locator(".citation").click();
  await expect(conclusion).toHaveClass(/open/);
  await conclusion.getByRole("link", { name: /^Message 3: open the .+ call in the transcript$/ }).click();
  await expect(page).toHaveURL(new RegExp(`/sessions/${SESSION_ID}[?](.*&)?msg=3`));
  const scroller = page.locator(".message-list-scroll");
  const selected = scroller.locator(".virtual-row.selected");
  await expect(selected).toHaveAttribute("data-index", "3");
  await expect(selected).toBeInViewport({ timeout: 10_000 });

  await page.reload({ waitUntil: "domcontentloaded" });
  await expect(page.locator(".message-list-scroll .virtual-row.selected")).toHaveAttribute(
    "data-index",
    "3",
  );

  detail = await openReport(page, 1280);
  const sequence = detail.locator(".tool-sequences-panel .sequence").first();
  await sequence.locator(".sequence-row").click();
  await expect(sequence.locator(".sequence-row")).toHaveAttribute("aria-expanded", "true");
  await sequence.getByRole("link", { name: "Message 1: open the Grep call in the transcript", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`/sessions/${SESSION_ID}[?](.*&)?msg=1`));
  await expect(page.locator(".message-list-scroll .virtual-row.selected")).toHaveAttribute(
    "data-index",
    "1",
  );
});
