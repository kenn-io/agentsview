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
