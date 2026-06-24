import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

function readZoom(page: import("@playwright/test").Page): Promise<string> {
  return page.evaluate(() =>
    document.documentElement.style.getPropertyValue("zoom"),
  );
}

test.describe("Appearance accessibility", () => {
  test("text size scales the UI on web without horizontal overflow", async ({
    page,
  }) => {
    await page.addInitScript(() => {
      localStorage.setItem("agentsview-font-scale", "130");
    });
    const sp = new SessionsPage(page);
    await sp.goto();

    expect(await readZoom(page)).toBe("1.3");

    const overflow = await page.evaluate(
      () =>
        document.documentElement.scrollWidth -
        document.documentElement.clientWidth,
    );
    expect(overflow).toBeLessThanOrEqual(2);

    await sp.selectFirstSession();
    await expect(sp.messageRows.first()).toBeVisible();
  });

  test("text size at 90% renders and scrolls the transcript", async ({
    page,
  }) => {
    await page.addInitScript(() => {
      localStorage.setItem("agentsview-font-scale", "90");
    });
    const sp = new SessionsPage(page);
    await sp.goto();

    expect(await readZoom(page)).toBe("0.9");

    await sp.selectFirstSession();
    await expect(sp.messageRows.first()).toBeVisible();
  });

  test("desktop window zoom and text size compose multiplicatively", async ({
    page,
  }) => {
    await page.addInitScript(() => {
      localStorage.setItem("agentsview-zoom-level", "150");
      localStorage.setItem("agentsview-font-scale", "120");
    });
    await page.goto("/?desktop");
    const sp = new SessionsPage(page);
    await expect(sp.sessionItems.first()).toBeVisible({ timeout: 5_000 });

    expect(await readZoom(page)).toBe("1.8");
  });

  test("high contrast applies the root class and overrides tokens", async ({
    page,
  }) => {
    await page.addInitScript(() => {
      localStorage.setItem("theme", "light");
      localStorage.setItem("agentsview-high-contrast", "true");
    });
    const sp = new SessionsPage(page);
    await sp.goto();

    const hasClass = await page.evaluate(() =>
      document.documentElement.classList.contains("high-contrast"),
    );
    expect(hasClass).toBe(true);

    // Browsers may normalize #000000 to #000; compare after stripping
    // leading # and zero-padding each channel to 6 hex digits.
    const textPrimary = await page.evaluate((): string => {
      const raw = getComputedStyle(document.documentElement)
        .getPropertyValue("--text-primary")
        .trim();
      // Expand shorthand #rgb → #rrggbb before comparing.
      if (/^#[0-9a-fA-F]{3}$/.test(raw)) {
        return "#" + raw[1]!.repeat(2) + raw[2]!.repeat(2) + raw[3]!.repeat(2);
      }
      return raw;
    });
    expect(textPrimary).toBe("#000000");
  });
});
