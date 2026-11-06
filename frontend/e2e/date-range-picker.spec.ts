import { expect, test } from "@playwright/test";

for (const theme of ["light", "dark"] as const) {
  test(`date presets are a second segmented control under the mode tabs in ${theme}`, async ({
    page,
  }) => {
    await page.addInitScript((t) => localStorage.setItem("theme", t), theme);
    await page.goto("/usage");
    await page.locator(".kit-date-range-picker__trigger").first().click();
    const panel = page.locator(".kit-date-range-picker__panel");
    await expect(panel).toBeVisible();

    for (const mode of [0, 1]) {
      await panel.locator(".kit-segmented__btn").nth(mode).click();
      const geometry = await panel.evaluate(async (el) => {
        // Switching modes fades the old and new selected fills; read them
        // once those transitions settle.
        await Promise.all(el.getAnimations({ subtree: true }).map((a) => a.finished));
        const track = el.querySelector(".kit-date-range-picker__tabs .kit-segmented")!;
        const presets = el.querySelector(".kit-date-range-picker__pills")!;
        const chosenMode = track.querySelector(".kit-segmented__btn.active")!;
        const otherMode = track.querySelector(".kit-segmented__btn:not(.active)")!;
        const chosenOption = presets.querySelector(".kit-date-range-picker__pill.active")!;
        const otherOption = presets.querySelector(".kit-date-range-picker__pill:not(.active)")!;
        const t = track.getBoundingClientRect();
        const p = presets.getBoundingClientRect();
        const style = (node: Element) => getComputedStyle(node);
        const corners = (node: Element) => {
          const s = style(node);
          return [s.borderTopLeftRadius, s.borderTopRightRadius, s.borderBottomRightRadius, s.borderBottomLeftRadius];
        };
        return {
          gap: p.top - t.bottom,
          left: p.left - t.left,
          right: t.right - p.right,
          trackCorners: corners(track),
          presetCorners: corners(presets),
          trackFill: style(track).backgroundColor,
          presetFill: style(presets).backgroundColor,
          modeButtonCorners: corners(chosenMode),
          optionCorners: corners(chosenOption),
          modeHeight: chosenMode.getBoundingClientRect().height,
          optionHeight: chosenOption.getBoundingClientRect().height,
          chosenMode: [style(chosenMode).backgroundColor, style(chosenMode).boxShadow, style(chosenMode).fontWeight],
          chosenOption: [style(chosenOption).backgroundColor, style(chosenOption).boxShadow, style(chosenOption).fontWeight],
          otherModeFill: style(otherMode).backgroundColor,
          otherOptionFill: style(otherOption).backgroundColor,
          ornaments: [getComputedStyle(presets, "::before").content, getComputedStyle(presets, "::after").content],
        };
      });
      const info = JSON.stringify(geometry);
      // Its own row below the mode tabs, the same width, not joined to them.
      expect(geometry.gap, info).toBeGreaterThanOrEqual(4);
      expect(Math.abs(geometry.left), info).toBeLessThanOrEqual(0.5);
      expect(Math.abs(geometry.right), info).toBeLessThanOrEqual(0.5);
      expect(geometry.ornaments, info).toEqual(["none", "none"]);
      // The same track and option shape as the mode tabs.
      expect(geometry.presetCorners, info).toEqual(geometry.trackCorners);
      expect(geometry.presetFill, info).toBe(geometry.trackFill);
      expect(geometry.optionCorners, info).toEqual(geometry.modeButtonCorners);
      expect(geometry.otherOptionFill, info).toBe(geometry.otherModeFill);
      // The chosen preset uses the same selected pill as the chosen mode.
      expect(geometry.chosenOption, info).toEqual(geometry.chosenMode);
      // Subordinate only by size: presets are no taller than the mode tabs.
      expect(geometry.optionHeight, info).toBeLessThan(geometry.modeHeight);
    }
  });
}
