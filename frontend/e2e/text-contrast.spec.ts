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

    test("treemap tile values and details stay readable on solid tiles", async ({ page }) => {
      await page.goto("/usage");
      const tiles = page.locator(".treemap-container .tile:not(.patterned)");
      await expect(tiles.first()).toBeVisible();
      const texts = page.locator(
        ".treemap-container .tile:not(.patterned) :is(.tile-label, .tile-label-sm, .tile-value, .tile-meta)",
      );
      const count = await texts.count();
      expect(count).toBeGreaterThan(0);
      for (let i = 0; i < count; i++) {
        const text = texts.nth(i);
        const paint = await text.evaluate((el) => {
          const tile = el.closest(".tile")!;
          const style = getComputedStyle(el);
          return {
            fill: style.fill,
            opacity: Number(style.fillOpacity),
            tile: getComputedStyle(tile.querySelector("rect")!).fill,
          };
        });
        const contrast = await renderedContrast(text, {
          foreground: paint.fill,
          background: paint.tile,
          opacity: paint.opacity,
        });
        expect(contrast.ratio, `tile text ${i} ${JSON.stringify(contrast)}`).toBeGreaterThanOrEqual(4.5);
      }
    });

    test("unclean status dots read as marks and the unclean count stays readable", async ({
      page,
    }) => {
      await page.goto("/sessions");
      const dots = page.locator(".kit-status-dot--unclean");
      await expect(dots.first()).toBeVisible();
      const dotCount = await dots.count();
      for (let i = 0; i < Math.min(dotCount, 6); i++) {
        const dot = dots.nth(i);
        const fill = await dot.evaluate((el) => getComputedStyle(el).backgroundColor);
        const contrast = await renderedContrast(dot.locator(".."), { foreground: fill });
        expect(contrast.ratio, `unclean dot ${i} ${JSON.stringify(contrast)}`).toBeGreaterThanOrEqual(3);
      }
      const pill = page.locator(".status-count-pill");
      await expect(pill).toBeVisible();
      const resting = await renderedContrast(pill);
      expect(resting.ratio, JSON.stringify(resting)).toBeGreaterThanOrEqual(4.5);
      await pill.hover();
      await expect.poll(async () => (await renderedContrast(pill)).background).not.toBe(resting.background);
      const hovered = await renderedContrast(pill);
      expect(hovered.ratio, JSON.stringify(hovered)).toBeGreaterThanOrEqual(4.5);
    });

    test("find overview ticks read as marks on their track", async ({ page }) => {
      const sp = new SessionsPage(page);
      await sp.goto();
      await sp.selectFirstSession();
      const modifier = await page.evaluate(() => (/Mac/.test(navigator.platform) ? "Meta" : "Control"));
      await page.keyboard.press(`${modifier}+f`);
      await page.locator(".kit-find-bar__input").fill("e");
      const rail = page.locator(".find-overview-rail");
      await expect(rail.locator(".tick.current")).toHaveCount(1);
      const track = await rail.locator(".track").evaluate((el) => getComputedStyle(el).fill);
      const ticks = await rail.locator(".tick").evaluateAll((els) =>
        [...new Set(els.map((el) => getComputedStyle(el).fill))],
      );
      expect(ticks.length).toBeGreaterThanOrEqual(2);
      for (const tick of ticks) {
        const contrast = await renderedContrast(rail, { foreground: tick, background: track });
        expect(contrast.ratio, JSON.stringify(contrast)).toBeGreaterThanOrEqual(3);
      }
    });

    test("popover edges keep 3:1 against the popover", async ({ page }) => {
      // Normal modes use a soft edge plus a shadow; only high contrast
      // promises a 3:1 boundary.
      test.skip(!highContrast, "high contrast only");
      const sp = new SessionsPage(page);
      await sp.goto();
      await sp.selectFirstSession();
      await page.locator(".view-trigger").click();
      const menu = page.locator(".kit-menu__content.kit-popover-card");
      await expect(menu).toBeVisible();
      const paint = await menu.evaluate((el) => {
        const style = getComputedStyle(el);
        return { edge: style.borderTopColor };
      });
      const contrast = await renderedContrast(menu, { foreground: paint.edge });
      expect(contrast.ratio, JSON.stringify(contrast)).toBeGreaterThanOrEqual(3);
    });

    test("the folded Other series reads as a mark in high contrast", async ({ page }) => {
      // Normal modes keep it a quiet tail behind the named series.
      test.skip(!highContrast, "high contrast only");
      await page.goto("/usage");
      const chart = page.locator(".treemap-container").first();
      await expect(chart).toBeVisible();
      const other = await chart.evaluate((el) => {
        const probe = document.createElement("span");
        probe.style.color = "var(--usage-series-other)";
        el.appendChild(probe);
        const color = getComputedStyle(probe).color;
        probe.remove();
        return color;
      });
      const contrast = await renderedContrast(chart, { foreground: other });
      expect(contrast.ratio, JSON.stringify(contrast)).toBeGreaterThanOrEqual(3);
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
