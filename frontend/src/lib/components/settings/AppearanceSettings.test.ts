import { cleanup, fireEvent, render } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import AppearanceSettings from "./AppearanceSettings.svelte";
import { SettingsService } from "../../api/generated/index";
import { settings } from "../../stores/settings.svelte.js";
import { ui } from "../../stores/ui.svelte.js";

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    SettingsService: {
      putApiV1Settings: vi.fn(),
    },
  };
});

const settingsService = SettingsService as unknown as {
  putApiV1Settings: ReturnType<typeof vi.fn>;
};

describe("AppearanceSettings", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    settings.chartPalette = "agentsview";
    settings.readOnly = false;
    settings.saving = false;
  });

  afterEach(() => {
    ui.setFontScale(100);
    if (ui.highContrast) ui.toggleHighContrast();
    settings.chartPalette = "agentsview";
    settings.readOnly = false;
    cleanup();
  });

  it("renders five text-size options and marks the active scale", () => {
    ui.setFontScale(110);
    const { getByRole } = render(AppearanceSettings);
    for (const pct of [90, 100, 110, 120, 130]) {
      expect(getByRole("radio", { name: `${pct}%` })).toBeTruthy();
    }
    expect(getByRole("radio", { name: "110%" }).getAttribute("aria-checked")).toBe("true");
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

  it("saves and confirms the selected chart palette", async () => {
    settingsService.putApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "matplotlib",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
    });
    const { getByRole } = render(AppearanceSettings);

    await fireEvent.click(getByRole("radio", { name: "Matplotlib" }));

    expect(settingsService.putApiV1Settings).toHaveBeenCalledWith({
      chart_palette: "matplotlib",
    });
    expect(getByRole("radio", { name: "Matplotlib" }).getAttribute("aria-checked")).toBe("true");
  });

  it("disables chart palette controls in read-only mode", () => {
    settings.readOnly = true;
    const { getByRole } = render(AppearanceSettings);

    expect((getByRole("radio", { name: "Agentsview" }) as HTMLButtonElement).disabled).toBe(true);
    expect((getByRole("radio", { name: "Matplotlib" }) as HTMLButtonElement).disabled).toBe(true);
  });
});
