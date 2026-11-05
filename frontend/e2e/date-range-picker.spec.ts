import { expect, test } from "@playwright/test";

for (const theme of ["light", "dark"] as const) {
  test(`date presets hang from the mode control as a narrower tray in ${theme}`, async ({
    page,
  }) => {
    await page.addInitScript((t) => localStorage.setItem("theme", t), theme);
    await page.goto("/usage");
    await page.locator(".kit-date-range-picker__trigger").first().click();
    const panel = page.locator(".kit-date-range-picker__panel");
    await expect(panel).toBeVisible();

    for (const mode of [0, 1]) {
      await panel.locator(".kit-segmented__btn").nth(mode).click();
      const geometry = await panel.evaluate((el) => {
        const track = el.querySelector(".kit-date-range-picker__tabs .kit-segmented")!;
        const tray = el.querySelector(".kit-date-range-picker__pills")!;
        const chosenMode = el.querySelector(".kit-segmented__btn.active")!;
        const chosenOption = tray.querySelector(".kit-date-range-picker__pill.active");
        const t = track.getBoundingClientRect();
        const r = tray.getBoundingClientRect();
        return {
          gap: r.top - t.bottom,
          insetLeft: r.left - t.left,
          insetRight: t.right - r.right,
          trayFill: getComputedStyle(tray).backgroundColor,
          trackFill: getComputedStyle(track).backgroundColor,
          modeShadow: getComputedStyle(chosenMode).boxShadow,
          optionShadow: chosenOption ? getComputedStyle(chosenOption).boxShadow : "none",
        };
      });
      // Attached to the track's bottom edge, inset from both ends, in the
      // same fill so the two read as one shape.
      expect(Math.abs(geometry.gap), JSON.stringify(geometry)).toBeLessThanOrEqual(0.5);
      expect(geometry.insetLeft).toBeGreaterThanOrEqual(8);
      expect(geometry.insetRight).toBeGreaterThanOrEqual(8);
      expect(geometry.trayFill).toBe(geometry.trackFill);
      // Only the chosen mode is raised; the chosen option is flat.
      expect(geometry.modeShadow).not.toBe("none");
      expect(geometry.optionShadow).toBe("none");
    }
  });
}
