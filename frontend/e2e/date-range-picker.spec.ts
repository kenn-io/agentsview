import { expect, test } from "@playwright/test";

for (const theme of ["light", "dark"] as const) {
  test(`date presets are a lower tier tucked under the mode tabs in ${theme}`, async ({
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
          presetBorders: [
            style(presets).borderTopWidth,
            style(presets).borderRightWidth,
            style(presets).borderBottomWidth,
            style(presets).borderLeftWidth,
          ],
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
      // Tucked under the mode track: flush with its bottom edge, inset so
      // the track overhangs it on both sides.
      expect(Math.abs(geometry.gap), info).toBeLessThanOrEqual(1);
      expect(geometry.left, info).toBeGreaterThanOrEqual(12);
      expect(geometry.left, info).toBeLessThanOrEqual(20);
      expect(geometry.right, info).toBeCloseTo(geometry.left, 0);
      // A straight square top with no ornament at the join; the bottom
      // corners use the track's radius.
      expect(geometry.ornaments, info).toEqual(["none", "none"]);
      expect(geometry.presetCorners.slice(0, 2), info).toEqual(["0px", "0px"]);
      expect(geometry.presetCorners.slice(2), info).toEqual(geometry.trackCorners.slice(2));
      expect(geometry.trackCorners[2], info).not.toBe("0px");
      // A lower tier: its own quieter fill, with a hairline on the sides and
      // bottom but none along the join.
      expect(geometry.presetFill, info).not.toBe(geometry.trackFill);
      expect(geometry.presetBorders[0], info).toBe("0px");
      expect(geometry.presetBorders.slice(1), info).toEqual(["1px", "1px", "1px"]);
      // Options shaped like the mode tabs, unselected ones flat.
      expect(geometry.optionCorners, info).toEqual(geometry.modeButtonCorners);
      expect(geometry.otherOptionFill, info).toBe(geometry.otherModeFill);
      // The chosen preset uses the same selected pill as the chosen mode.
      expect(geometry.chosenOption, info).toEqual(geometry.chosenMode);
      // Subordinate in size too: preset options are shorter than the mode tabs.
      expect(geometry.optionHeight, info).toBeLessThan(geometry.modeHeight);
    }
  });
}
