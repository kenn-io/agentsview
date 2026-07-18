import { cleanup, fireEvent, render } from "@testing-library/svelte";
import { afterEach, describe, expect, it } from "vitest";

import AppearanceSettings from "./AppearanceSettings.svelte";
import { ui } from "../../stores/ui.svelte.js";

describe("AppearanceSettings", () => {
  afterEach(() => {
    ui.setFontScale(100);
    if (ui.highContrast) ui.toggleHighContrast();
    cleanup();
  });

  it("renders five text-size options and marks the active scale", () => {
    ui.setFontScale(110);
    const { getByRole } = render(AppearanceSettings);
    for (const pct of [90, 100, 110, 120, 130]) {
      expect(getByRole("radio", { name: `${pct}%` })).toBeTruthy();
    }
    expect(
      getByRole("radio", { name: "110%" }).getAttribute("aria-checked"),
    ).toBe("true");
  });

  it("changes the font scale when an option is clicked", async () => {
    const { getByRole } = render(AppearanceSettings);
    await fireEvent.click(getByRole("radio", { name: "120%" }));
    expect(ui.fontScale).toBe(120);
  });

  it("toggles high contrast", async () => {
    const { getByRole } = render(AppearanceSettings);
    expect(ui.highContrast).toBe(false);
    await fireEvent.click(getByRole("button", { name: "Off" }));
    expect(ui.highContrast).toBe(true);
  });
});
