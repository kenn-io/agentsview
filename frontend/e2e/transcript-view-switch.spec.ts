import { expect, test, type Page } from "@playwright/test";

const SESSION = "test-session-xlarge-5500";

// The fixture session alternates prompts and answers, so both content modes
// show the same rows. Turning the prompt in every other turn into an
// assistant message gives each turn three assistant messages; "Prompts and
// final answers" keeps only the last, so it shows half as many rows.
async function withMultiStepTurns(page: Page) {
  await page.route(/\/api\/v1\/sessions\/[^/]+\/messages(?:\?.*)?$/, async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    for (const message of body.messages) {
      if (message.ordinal % 4 === 2) message.role = "assistant";
    }
    await route.fulfill({ response, json: body });
  });
}

type Layout = {
  scrollHeight: number;
  visibleRows: number;
  problems: string[];
};

// Rows on screen, and every pair of neighboring rows that overlaps or
// leaves a gap.
async function transcriptLayout(page: Page): Promise<Layout> {
  return page.locator(".message-list-scroll").evaluate((scroller) => {
    const view = scroller.getBoundingClientRect();
    const rows = [...scroller.querySelectorAll<HTMLElement>(".virtual-row")]
      .map((el) => ({ index: Number(el.dataset.index), rect: el.getBoundingClientRect() }))
      .sort((a, b) => a.index - b.index);
    const problems: string[] = [];
    let visibleRows = 0;
    for (const [i, row] of rows.entries()) {
      if (row.rect.bottom <= view.top || row.rect.top >= view.bottom) continue;
      visibleRows++;
      const next = rows[i + 1];
      if (!next || next.index !== row.index + 1) continue;
      const step = Math.round(next.rect.top - row.rect.bottom);
      if (Math.abs(step) > 1) problems.push(`rows ${row.index}/${next.index}: ${step}px`);
    }
    return { scrollHeight: scroller.scrollHeight, visibleRows, problems };
  });
}

async function expectHealthy(page: Page, check: (layout: Layout) => boolean, label: string) {
  await expect
    .poll(async () => {
      const layout = await transcriptLayout(page);
      return layout.visibleRows > 0 && layout.problems.length === 0 && check(layout)
        ? "ok"
        : JSON.stringify(layout);
    }, { message: label })
    .toBe("ok");
}

test("switching content and layout with the View menu open keeps the transcript laid out", async ({ page }) => {
  await withMultiStepTurns(page);
  await page.goto(`/sessions/${SESSION}`);
  const scroller = page.locator(".message-list-scroll");
  await expect(scroller).toHaveAttribute("data-loaded", "true");
  await expect(page.locator(".virtual-row").first()).toBeVisible();

  // Deep enough that the shorter mode's rows end above this offset.
  await scroller.evaluate((el) => {
    el.scrollTop = el.scrollHeight * 0.8;
  });
  await expectHealthy(page, () => true, "Everything before switching");
  const everythingHeight = (await transcriptLayout(page)).scrollHeight;

  await page.locator(".session-breadcrumb .view-trigger").click();
  const pick = (name: string) =>
    page.getByRole("menuitemradio", { name: new RegExp(`^${name}`) }).click();

  await pick("Prompts and final answers");
  await expectHealthy(
    page,
    (layout) => layout.scrollHeight < everythingHeight * 0.75,
    "Prompts and final answers",
  );
  const promptsHeight = (await transcriptLayout(page)).scrollHeight;

  await pick("Everything");
  await expectHealthy(
    page,
    (layout) => layout.scrollHeight > promptsHeight * 1.5,
    "Everything again",
  );

  for (const layout of ["Compact", "Skim", "Stream", "Default"]) {
    await pick(layout);
    await expectHealthy(page, () => true, layout);
  }

  // Every pick above kept the menu open.
  await expect(page.getByRole("menuitemradio", { name: /^Everything/ })).toBeVisible();
});
