import { expect, test, type Page } from "@playwright/test";

async function openPicker(page: Page, theme: "light" | "dark") {
  await page.addInitScript((t) => localStorage.setItem("theme", t), theme);
  await page.goto("/usage");
  const trigger = page.locator(".kit-date-range-picker__trigger").first();
  await trigger.click();
  const panel = page.locator(".kit-date-range-picker__panel");
  await expect(panel).toBeVisible();
  return { trigger, panel };
}

for (const theme of ["light", "dark"] as const) {
  test(`date presets are a lower tier tucked under the mode tabs in ${theme}`, async ({
    page,
  }) => {
    const { panel } = await openPicker(page, theme);

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
        // A segmented control elsewhere on the page (Cost / Tokens, the
        // chart style toggle) sets the shared shape.
        const reference = [...document.querySelectorAll(".kit-segmented")].find(
          (node) => !el.contains(node),
        )!;
        const referenceButton = reference.querySelector(".kit-segmented__btn")!;
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
          referenceTrackCorners: corners(reference),
          presetCorners: corners(presets),
          trackFill: style(track).backgroundColor,
          presetFill: style(presets).backgroundColor,
          presetBorders: [
            style(presets).borderTopWidth,
            style(presets).borderRightWidth,
            style(presets).borderBottomWidth,
            style(presets).borderLeftWidth,
          ],
          presetShadow: style(presets).boxShadow,
          roomAbove: chosenOption.getBoundingClientRect().top - p.top,
          roomBelow: p.bottom - chosenOption.getBoundingClientRect().bottom,
          modeButtonCorners: corners(chosenMode),
          referenceButtonCorners: corners(referenceButton),
          optionCorners: corners(chosenOption),
          modeHeight: chosenMode.getBoundingClientRect().height,
          optionHeight: chosenOption.getBoundingClientRect().height,
          modeFontSize: parseFloat(style(chosenMode).fontSize),
          optionFontSize: parseFloat(style(chosenOption).fontSize),
          chosenMode: [style(chosenMode).backgroundColor, style(chosenMode).boxShadow, style(chosenMode).fontWeight],
          chosenOption: [style(chosenOption).backgroundColor, style(chosenOption).boxShadow, style(chosenOption).fontWeight],
          otherModeFill: style(otherMode).backgroundColor,
          otherOptionFill: style(otherOption).backgroundColor,
          ornaments: [presets, track, el.querySelector(".kit-date-range-picker__tabs")!].flatMap((node) => [
            getComputedStyle(node, "::before").content,
            getComputedStyle(node, "::after").content,
          ]),
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
      for (const ornament of geometry.ornaments) {
        expect(["none", "normal"], info).toContain(ornament);
      }
      expect(geometry.presetCorners.slice(0, 2), info).toEqual(["0px", "0px"]);
      expect(geometry.presetCorners.slice(2), info).toEqual(geometry.trackCorners.slice(2));
      expect(geometry.trackCorners[2], info).not.toBe("0px");
      // The mode track and its buttons have the shape of the app's other
      // segmented controls.
      expect(geometry.trackCorners, info).toEqual(geometry.referenceTrackCorners);
      expect(geometry.modeButtonCorners, info).toEqual(geometry.referenceButtonCorners);
      // A lower tier: its own quieter fill inside a hairline ring, with one
      // contact line painted over the ring's top row so the line runs the
      // full width and the top corners stay square. Borders would add a
      // mitred corner pixel, so the tier has none.
      expect(geometry.presetFill, info).not.toBe(geometry.trackFill);
      expect(geometry.presetBorders, info).toEqual(["0px", "0px", "0px", "0px"]);
      const [contactLine, ring, ...extra] = geometry.presetShadow.split(/,(?![^(]*\))/).map((s) => s.trim());
      expect(extra, info).toEqual([]);
      expect(contactLine, info).toMatch(/ 0px 1px 0px 0px inset$/);
      expect(ring, info).toMatch(/ 0px 0px 0px 1px inset$/);
      // Tier fill shows above and below the chosen preset.
      expect(geometry.roomAbove, info).toBeGreaterThanOrEqual(3);
      expect(geometry.roomBelow, info).toBeGreaterThanOrEqual(3);
      // Options shaped like the mode tabs, unselected ones flat.
      expect(geometry.optionCorners, info).toEqual(geometry.modeButtonCorners);
      expect(geometry.otherOptionFill, info).toBe(geometry.otherModeFill);
      // The chosen preset uses the same selected pill as the chosen mode.
      expect(geometry.chosenOption, info).toEqual(geometry.chosenMode);
      // Subordinate in size too: preset options are shorter than the mode
      // tabs and set in smaller type.
      expect(geometry.optionHeight, info).toBeLessThan(geometry.modeHeight);
      expect(geometry.optionFontSize, info).toBeLessThan(geometry.modeFontSize);
    }
  });
}

test("a picker that opens upward keeps the preset tier clear of its trigger", async ({ page }) => {
  // Put the trigger near the bottom of the window so the panel has no room
  // below it and opens upward.
  await page.addInitScript(() => {
    document.addEventListener("DOMContentLoaded", () => {
      const style = document.createElement("style");
      style.textContent = ".kit-date-range-picker { position: fixed !important; left: 48px; bottom: 20px; top: auto; z-index: 50; }";
      document.head.append(style);
    });
  });
  const { trigger, panel } = await openPicker(page, "light");

  for (const mode of [0, 1]) {
    await panel.locator(".kit-segmented__btn").nth(mode).click();
    // The panel re-anchors on the next frame after its height changes.
    const bottoms = async () => {
      const triggerBox = (await trigger.boundingBox())!;
      const panelBox = (await panel.boundingBox())!;
      const presetBox = (await panel.locator(".kit-date-range-picker__pills").boundingBox())!;
      return {
        triggerTop: triggerBox.y,
        panelBottom: panelBox.y + panelBox.height,
        presetBottom: presetBox.y + presetBox.height,
      };
    };
    await expect.poll(async () => {
      const b = await bottoms();
      return b.panelBottom <= b.triggerTop;
    }).toBe(true);
    const b = await bottoms();
    const info = JSON.stringify(b);
    // The preset tier stays inside the panel, so it never reaches the
    // trigger below.
    expect(b.presetBottom, info).toBeLessThanOrEqual(b.panelBottom);
    expect(b.presetBottom, info).toBeLessThan(b.triggerTop);
  }
});
