import AxeBuilder from "@axe-core/playwright";
import { test, expect, type Page } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

// PRODUCT.md sets WCAG 2.2 AA as the target; these tags select its A and AA
// success criteria. The spec fails on serious and critical violations.
const WCAG_TAGS = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"];

type Theme = "light" | "dark";

interface Finding {
  rule: string;
  impact: string | null | undefined;
  help: string;
  targets: string[];
}

// Every loading marker a main route shows before its data arrives.
const LOADING = [
  '[aria-busy="true"]',
  '[class*="skeleton"]',
  ".settings-loading",
  ".re-loading",
  ".loading-state",
  ".workspace-loading",
  ".archive-state",
  ".kit-refresh-control__btn.querying",
].join(", ");

/** Waits until nothing reports loading and the DOM has stopped changing for
 * half a second, so axe sees the loaded page rather than a placeholder. A
 * loading marker can appear during the quiet wait, so check again after it. */
async function waitForSettledPage(page: Page): Promise<void> {
  const loading = page.locator(LOADING);
  await expect(async () => {
    await expect(loading).toHaveCount(0);
    await waitForQuietDom(page);
    await expect(loading).toHaveCount(0, { timeout: 0 });
  }).toPass();
}

async function waitForQuietDom(page: Page): Promise<void> {
  await page.evaluate(
    () =>
      new Promise<void>((resolve) => {
        let timer = setTimeout(done, 500);
        const observer = new MutationObserver(() => {
          clearTimeout(timer);
          timer = setTimeout(done, 500);
        });
        function done() {
          observer.disconnect();
          resolve();
        }
        observer.observe(document.body, {
          subtree: true,
          childList: true,
          attributes: true,
          characterData: true,
        });
      }),
  );
}

// kit-ui's StatusDot puts aria-label on a role-less <span>. DESIGN.md tracks
// it as a kit-ui gap; drop this filter once kit-ui gives the dot role="img".
function isKnownKitUiGap(rule: string, target: string): boolean {
  return rule === "aria-prohibited-attr" && target.includes(".kit-status-dot");
}

async function seriousViolations(page: Page): Promise<Finding[]> {
  await waitForSettledPage(page);
  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  return results.violations
    .filter((v) => v.impact === "serious" || v.impact === "critical")
    .map((v) => ({
      rule: v.id,
      impact: v.impact,
      help: v.help,
      targets: v.nodes
        .map((node) => node.target.join(" "))
        .filter((target) => !isKnownKitUiGap(v.id, target)),
    }))
    .filter((finding) => finding.targets.length > 0);
}

async function useTheme(page: Page, theme: Theme): Promise<void> {
  await page.addInitScript((value) => localStorage.setItem("theme", value), theme);
}

const ROUTES = [
  "usage",
  "token-usage",
  "activity",
  "trends",
  "recall",
  "quality",
  "friction",
  "pinned",
  "trash",
  "recent-edits",
  "data",
  "settings",
];

for (const theme of ["light", "dark"] as const) {
  test.describe(`axe on main routes (${theme})`, () => {
    // axe evaluates the DOM and computed styles, which match across engines
    // for these checks; one engine keeps the suite fast.
    test.skip(({ browserName }) => browserName !== "chromium", "axe runs on Chromium only");

    test.beforeEach(async ({ page }) => {
      await useTheme(page, theme);
    });

    test("sessions list and analytics", async ({ page }) => {
      const sp = new SessionsPage(page);
      await sp.goto();
      await expect(sp.analyticsPage).toBeVisible();
      expect(await seriousViolations(page)).toEqual([]);
    });

    test("session transcript", async ({ page }) => {
      const sp = new SessionsPage(page);
      await sp.goto();
      await sp.selectFirstSession();
      expect(await seriousViolations(page)).toEqual([]);
    });

    test("command palette", async ({ page }) => {
      const sp = new SessionsPage(page);
      await sp.goto();
      await page.keyboard.press("ControlOrMeta+k");
      await expect(page.locator(".palette-input")).toBeVisible();
      await page.locator(".palette-input").fill("test");
      await expect(page.locator(".palette-item").first()).toBeVisible();
      expect(await seriousViolations(page)).toEqual([]);
    });

    for (const route of ROUTES) {
      test(route, async ({ page }) => {
        await page.goto(`/${route}`);
        await expect(page.locator(".kit-top-bar")).toBeVisible();
        expect(await seriousViolations(page)).toEqual([]);
      });
    }
  });
}
