import { expect, test } from "@playwright/test";
import { clickNavTab, expectActiveNavTab } from "./helpers/nav";

const LOCALE_STORAGE_KEY = "agentsview-locale";

test.describe("Spanish from the browser language", () => {
  test.use({ locale: "es-MX" });

  test("picks Spanish when no locale is stored", async ({ page }) => {
    await page.goto("/");
    await expectActiveNavTab(page, "Sesiones");
    await expect(page.locator(".session-item").first()).toBeVisible();
  });

  test("keeps the active destination during navigation reflow", async ({ page }) => {
    await page.setViewportSize({ width: 2800, height: 900 });
    await page.goto("/");
    await expect(page.locator(".kit-top-bar__tabs")).toBeVisible();

    for (const width of [900, 2800, 900, 2800, 900, 2800]) {
      await Promise.all([
        expectActiveNavTab(page, "Sesiones"),
        page.setViewportSize({ width, height: 900 }),
      ]);
      await expect(
        page.locator(width === 900 ? ".kit-top-bar__nav-select" : ".kit-top-bar__tabs"),
      ).toBeVisible();
      await expectActiveNavTab(page, "Sesiones");
    }
  });
});

test.describe("Spanish from Settings", () => {
  test.use({ locale: "en-US" });

  test("switching the interface language renders Spanish chrome, settings, and sessions", async ({
    page,
  }) => {
    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: "Settings" }).first()).toBeVisible();
    await page.locator('nav[aria-label="Settings"] button', { hasText: "Language" }).click();

    await page.locator('button[title="Interface language"]').click();
    await page.getByRole("option", { name: "Español" }).click();

    await expect(page.getByRole("heading", { name: "Configuración" }).first()).toBeVisible();
    await page.locator('nav[aria-label="Configuración"] button', { hasText: "Idioma" }).click();
    await expect(page.locator('button[title="Idioma de la interfaz"]')).toBeVisible();
    expect(await page.evaluate((key) => localStorage.getItem(key), LOCALE_STORAGE_KEY)).toBe("es");

    await clickNavTab(page, "Sesiones");
    await expectActiveNavTab(page, "Sesiones");
    await expect(page.locator(".session-item").first()).toBeVisible();
    await expect(page.locator("body")).not.toContainText("Search sessions");
  });
});
