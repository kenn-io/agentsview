import { test, expect } from "@playwright/test";

test.describe("Usage attribution touch", () => {
  test.use({ hasTouch: true, isMobile: true, viewport: { width: 900, height: 1000 } });

  for (const view of ["treemap", "list"] as const) {
    test(`double tap opens a project in ${view}`, async ({ page }) => {
      await page.goto("/usage");
      const panel = page.locator(".attribution-panel");
      if (view === "list") await panel.getByRole("button", { name: "List", exact: true }).tap();
      const rows = panel.locator(view === "treemap" ? ".tile" : ".list-row");
      await expect(rows.first()).toBeVisible();
      const count = await rows.count();
      expect(count).toBeGreaterThan(1);
      const colors = await rows.evaluateAll((items) => items.map((item) => item.querySelector("rect")?.getAttribute("fill") ?? item.querySelector(".list-dot")?.getAttribute("style")));
      await rows.first().tap();
      await expect(rows.first()).toHaveAttribute("aria-pressed", "true");
      await expect(rows).toHaveCount(count);
      await expect(panel.locator(".dimmed").first()).toBeVisible();
      expect(await rows.evaluateAll((items) => items.map((item) => item.querySelector("rect")?.getAttribute("fill") ?? item.querySelector(".list-dot")?.getAttribute("style")))).toEqual(colors);
      // Separate the single tap assertion from the next double tap gesture.
      await page.waitForTimeout(600);
      const box = await rows.first().boundingBox();
      expect(box).not.toBeNull();
      await page.touchscreen.tap(box!.x + box!.width / 2, box!.y + box!.height / 2);
      await page.touchscreen.tap(box!.x + box!.width / 2, box!.y + box!.height / 2);
      await expect(panel.getByRole("button", { name: "All projects" })).toBeVisible();
      await panel.getByRole("button", { name: "All projects" }).tap();
      await expect(rows).toHaveCount(count);
      await expect(rows.first()).toHaveAttribute("aria-pressed", "true");
    });
  }
});
