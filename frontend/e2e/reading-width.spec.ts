import { expect, test, type Page } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

// Wide enough that Default and Wider both fit inside the transcript pane, so
// each choice produces a distinct column width.
const VIEWPORT = { width: 1900, height: 900 };

async function chooseReadingWidth(page: Page, label: string) {
  await page.goto("/settings");
  const group = page.getByRole("radiogroup", { name: "Reading width" });
  await expect(group).toBeVisible();
  const option = group.getByRole("radio", { name: label, exact: true });
  await option.click();
  await expect(option).toHaveAttribute("aria-checked", "true");
}

async function openSession(page: Page) {
  const sp = new SessionsPage(page);
  await sp.goto();
  await sp.selectFirstSession();
}

/** The first transcript card's width and the row it centers in. */
async function columnGeometry(page: Page) {
  return page
    .locator(".virtual-row")
    .first()
    .evaluate((row) => {
      const card = row.firstElementChild as HTMLElement;
      const style = getComputedStyle(row);
      const rowContent =
        row.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight);
      const cardBox = card.getBoundingClientRect();
      const title = document.querySelector(".session-breadcrumb");
      const header = title ? getComputedStyle(title) : null;
      return {
        card: cardBox.width,
        cardLeft: cardBox.left,
        rowContent,
        headerPaddingLeft: header ? parseFloat(header.paddingLeft) : 0,
        headerLeft: title?.getBoundingClientRect().left ?? 0,
      };
    });
}

test.describe("Reading width", () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(VIEWPORT);
  });

  test("each choice sets the transcript and header column, and persists", async ({ page }) => {
    await openSession(page);
    const base = await columnGeometry(page);
    expect(base.card).toBeCloseTo(880, 0);
    // The header's content starts on the card's left edge.
    expect(base.headerLeft + base.headerPaddingLeft).toBeCloseTo(base.cardLeft, 0);

    await chooseReadingWidth(page, "Wider");
    await openSession(page);
    const wider = await columnGeometry(page);
    expect(wider.card).toBeCloseTo(1120, 0);
    expect(wider.headerLeft + wider.headerPaddingLeft).toBeCloseTo(wider.cardLeft, 0);

    await chooseReadingWidth(page, "Full");
    await openSession(page);
    const full = await columnGeometry(page);
    expect(full.card).toBeGreaterThan(1120);
    expect(full.card).toBeCloseTo(full.rowContent, 0);

    // The choice survives a reload and stays selected in settings.
    await page.reload();
    await expect(page.locator(".virtual-row").first()).toBeVisible();
    const reloaded = await columnGeometry(page);
    expect(reloaded.card).toBeCloseTo(full.card, 0);
    await page.goto("/settings");
    await expect(
      page
        .getByRole("radiogroup", { name: "Reading width" })
        .getByRole("radio", { name: "Full", exact: true }),
    ).toHaveAttribute("aria-checked", "true");

    // Default restores the shared 880px column.
    await chooseReadingWidth(page, "Default");
    await openSession(page);
    expect((await columnGeometry(page)).card).toBeCloseTo(880, 0);
  });
});
