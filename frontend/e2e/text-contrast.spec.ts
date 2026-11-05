import { expect, test, type Page } from "@playwright/test";
import { placeholderColor, renderedContrast, useTheme } from "./helpers/contrast";
import { SessionsPage } from "./pages/sessions-page";

const THEMES = [
  ["light", false],
  ["dark", false],
  ["light", true],
  ["dark", true],
] as const;

function themeName(mode: string, highContrast: boolean) {
  return highContrast ? `${mode} high contrast` : mode;
}

// Gives every listed and fetched session the same health grade.
async function withHealthGrade(page: Page, grade: string) {
  const patch = (session: Record<string, unknown>) => ({ ...session, health_grade: grade });
  await page.route(/\/api\/v1\/sessions(\/[^/?]+)?(\?.*)?$/, async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    const json = Array.isArray(body?.sessions)
      ? { ...body, sessions: body.sessions.map(patch) }
      : body && typeof body === "object" && "agent" in body
        ? patch(body)
        : body;
    await route.fulfill({ response, json });
  });
}

for (const [mode, highContrast] of THEMES) {
  test.describe(`text contrast in ${themeName(mode, highContrast)}`, () => {
    test.beforeEach(async ({ page }) => {
      await useTheme(page, mode, highContrast);
      await page.setViewportSize({ width: 1400, height: 900 });
    });

    test("settings nav descriptions stay readable on resting and selected rows", async ({
      page,
    }) => {
      await page.goto("/settings");
      const summaries = page.locator(".kit-settings__nav-summary");
      await expect(summaries.first()).toBeVisible();
      const count = await summaries.count();
      expect(count).toBeGreaterThan(1);
      let sawSelected = false;
      for (let i = 0; i < count; i++) {
        const summary = summaries.nth(i);
        if (await summary.evaluate((el) => !!el.closest("[aria-current]"))) sawSelected = true;
        const contrast = await renderedContrast(summary);
        expect(contrast.ratio, `nav summary ${i} ${JSON.stringify(contrast)}`).toBeGreaterThanOrEqual(4.5);
      }
      expect(sawSelected).toBe(true);
    });

    test("settings placeholders stay readable", async ({ page }) => {
      await page.goto("/settings");
      await page.locator(".kit-settings__nav").getByText("Session providers").first().click();
      const inputs = page.locator(".kit-settings__panel input.kit-text-input__control[placeholder]:enabled");
      await expect(inputs.first()).toBeVisible();
      const input = inputs.first();
      const contrast = await renderedContrast(input, {
        foreground: await placeholderColor(page, input),
      });
      expect(contrast.ratio, JSON.stringify(contrast)).toBeGreaterThanOrEqual(4.5);
    });

    for (const grade of ["A", "B", "C", "F"]) {
      test(`the ${grade} health grade stays readable at rest and on hover`, async ({ page }) => {
        await withHealthGrade(page, grade);
        const sp = new SessionsPage(page);
        await sp.goto();
        await sp.selectFirstSession();
        const badge = page.locator(".session-breadcrumb .grade-badge");
        await expect(badge).toHaveText(grade);
        const resting = await renderedContrast(badge);
        expect(resting.ratio, JSON.stringify(resting)).toBeGreaterThanOrEqual(4.5);
        await badge.hover();
        await expect
          .poll(async () => (await renderedContrast(badge)).background)
          .not.toBe(resting.background);
        const hovered = await renderedContrast(badge);
        expect(hovered.ratio, JSON.stringify(hovered)).toBeGreaterThanOrEqual(4.5);
      });
    }
  });
}
