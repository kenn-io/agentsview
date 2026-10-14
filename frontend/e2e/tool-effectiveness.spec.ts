import { test, expect, type Page } from "@playwright/test";

const SESSION_ID = "test-session-tool-sequences";

// The layout fixture names a long made-up MCP tool to test row widths, so its citations aren't
// tied to the seeded transcript. The navigation test builds an accurate report instead.
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
      { reason: "budget", field: "result", count: 1, kept_bytes: 12 },
      { reason: "previews", count: 1, kept_chars: 800 },
    ],
    cited_calls: [
      {
        ordinal: 1,
        call_index: 0,
        call_fingerprint: "0000aaaa",
        tool_name: "Grep",
        input_preview: '{"pattern":"config"}',
        outcome: "empty",
        result_bytes: 16,
        message_calls: 1,
      },
      {
        ordinal: 2,
        call_index: 0,
        call_fingerprint: "0000bbbb",
        tool_name: "Grep",
        input_preview: '{"pattern":"config"}',
        outcome: "empty",
        result_bytes: 16,
        message_calls: 1,
      },
      {
        ordinal: 3,
        call_index: 0,
        call_fingerprint: "0000cccc",
        tool_name: "mcp__onemcp__context7_1mcp_resolve_library_id",
        input_preview: '{"file_path":"app/config.json"}',
        outcome: "content",
        result_bytes: 16,
        result_kept_bytes: 12,
        message_calls: 1,
      },
    ],
  }),
  created_at: "2026-04-26T12:00:00Z",
};

// The seeded calls, with fingerprints of each call's tool name and input.
const SEEDED_CALLS = [
  {
    ordinal: 1,
    tool_use_id: "grep-1",
    call_fingerprint: "a82053b1",
    tool_name: "Grep",
    input_preview: '{"pattern":"config"}',
    outcome: "empty",
    result_bytes: 16,
  },
  {
    ordinal: 2,
    tool_use_id: "grep-2",
    call_fingerprint: "a82053b1",
    tool_name: "Grep",
    input_preview: '{"pattern":"config"}',
    outcome: "empty",
    result_bytes: 16,
  },
  {
    ordinal: 3,
    tool_use_id: "read-1",
    call_fingerprint: "7111f37f",
    tool_name: "Read",
    input_preview: '{"file_path":"app/config.json"}',
    outcome: "content",
    result_bytes: 16,
  },
];

/** A report whose citations name the seeded calls and the transcript revision they came from. */
function seededReport(transcriptRevision: string) {
  const structured = JSON.parse(report.structured_json);
  return {
    ...report,
    id: 78,
    structured_json: JSON.stringify({
      ...structured,
      omissions: [],
      transcript_revision: transcriptRevision,
      termination_status: "clean",
      cited_calls: SEEDED_CALLS.map((call) => ({ ...call, call_index: 0, message_calls: 1 })),
    }),
  };
}

function serveReports(page: Page, reports: object[]) {
  return page.route("**/api/v1/insights", (route) =>
    route.fulfill({ json: { insights: reports } }),
  );
}

async function openReport(page: Page, width: number, id = report.id) {
  await page.setViewportSize({ width, height: 900 });
  await page.goto(`/recall?tab=generated&insight=${id}`, { waitUntil: "domcontentloaded" });
  const detail = page.locator(".generated-detail");
  await expect(detail.locator(".tool-sequences-panel")).toBeVisible({ timeout: 10_000 });
  return detail;
}

test("saved tool-effectiveness report keeps conclusions, sequences, and omissions at each width", async ({
  page,
}, testInfo) => {
  await serveReports(page, [report]);
  for (const width of [1280, 768, 400]) {
    const detail = await openReport(page, width);
    // Text doesn't change with width, so check it once; layout is checked at every width.
    if (width === 1280) {
      await expect(detail.getByRole("heading", { name: "Model assessment" })).toBeVisible();
      await expect(detail.getByRole("heading", { name: "Observed tool sequences" })).toBeVisible();
      await expect(detail).toContainText("The second Grep repeated an empty search.");
      await expect(detail).toContainText("1 message shown as a preview of 800 characters");
      await expect(detail).toContainText("3 of 3 calls cited");
      await expect(detail.locator("[data-testid=tool-effectiveness-session-changed]")).toHaveCount(
        0,
      );
      await expect(detail.locator(".citations .citation")).toHaveText([
        /Grep\s*config\s*· Message 1/,
        /Grep\s*config\s*· Message 2/,
        /mcp__onemcp__context7_1mcp_resolve_library_id\s*app\/config\.json\s*· Message 3/,
      ]);
    }

    const evidence = detail.locator(".conclusion").nth(1);
    const citationInput = await evidence.locator(".citation .citation-input").boundingBox();
    expect(citationInput!.width).toBeGreaterThan(20);
    await evidence.locator(".citation").click();
    const row = evidence.locator(".call");
    if (width === 1280) await expect(row).toContainText("model saw 12 bytes");
    else await expect(row).toBeVisible();
    const [input, result, jump] = await Promise.all(
      [".input", ".res", ".jump"].map((s) => row.locator(s).boundingBox()),
    );
    // The long tool name must not squeeze the input preview out.
    expect(input!.width).toBeGreaterThan(40);
    const overlaps = (a: typeof result, b: typeof result) =>
      a!.x < b!.x + b!.width &&
      b!.x < a!.x + a!.width &&
      a!.y < b!.y + b!.height &&
      b!.y < a!.y + a!.height;
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

test("cited calls and sequence calls open the transcript at the cited call", async ({ page }) => {
  const session = await (await page.request.get(`/api/v1/sessions/${SESSION_ID}`)).json();
  const seeded = seededReport(session.transcript_revision);
  await serveReports(page, [seeded]);

  let detail = await openReport(page, 1280, seeded.id);
  // The report matches the transcript it cites, so nothing reads as stale.
  await expect(detail.locator("[data-testid=tool-effectiveness-session-changed]")).toHaveCount(0);
  const conclusion = detail.locator(".conclusion").nth(1);
  await conclusion.locator(".citation").click();
  await expect(conclusion).toHaveClass(/open/);
  await conclusion
    .getByRole("link", { name: "Message 3: open the Read call in the transcript", exact: true })
    .click();
  // The link names the call by tool ID and fingerprint, so the transcript can check it.
  await expect(page).toHaveURL(new RegExp(`/sessions/${SESSION_ID}[?](.*&)?msg=3(&|$)`));
  const url = new URL(page.url());
  expect(url.searchParams.get("tool_use_id")).toBe("read-1");
  expect(url.searchParams.get("fp")).toBe("7111f37f");
  const selected = page.locator(".message-list-scroll .virtual-row.selected");
  await expect(selected).toHaveAttribute("data-index", "3");
  await expect(selected).toBeInViewport({ timeout: 10_000 });
  await expect(selected).toContainText("app/config.json");

  // A reload reads the call back from the URL and checks it again.
  await page.reload({ waitUntil: "domcontentloaded" });
  await expect(selected).toHaveAttribute("data-index", "3");
  await expect(selected).toContainText("app/config.json");

  detail = await openReport(page, 1280, seeded.id);
  const sequence = detail.locator(".tool-sequences-panel .sequence").first();
  await sequence.locator(".sequence-row").click();
  await expect(sequence.locator(".sequence-row")).toHaveAttribute("aria-expanded", "true");
  await sequence
    .getByRole("link", { name: "Message 1: open the Grep call in the transcript", exact: true })
    .click();
  await expect(page).toHaveURL(new RegExp(`/sessions/${SESSION_ID}[?](.*&)?msg=1(&|$)`));
  expect(new URL(page.url()).searchParams.get("fp")).toBe("a82053b1");
  await expect(selected).toHaveAttribute("data-index", "1");
});

test("a link whose call no longer matches its fingerprint selects nothing", async ({ page }) => {
  const selected = page.locator(".message-list-scroll .virtual-row.selected");
  // The matching fingerprint opens the call, so the refusal below comes from the check.
  await page.goto(`/sessions/${SESSION_ID}?msg=3&call=0&tool_use_id=read-1&fp=7111f37f`, {
    waitUntil: "domcontentloaded",
  });
  await expect(selected).toHaveAttribute("data-index", "3", { timeout: 10_000 });

  // read-1 is still at message 3, but its tool name and input differ from what the link recorded.
  await page.goto(`/sessions/${SESSION_ID}?msg=3&call=0&tool_use_id=read-1&fp=00000000`, {
    waitUntil: "domcontentloaded",
  });
  await expect(page.locator(".message-list-scroll .virtual-row").first()).toBeVisible({
    timeout: 10_000,
  });
  await expect(selected).toHaveCount(0);
});
