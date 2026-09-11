import { cleanup, fireEvent, render, waitFor } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import AppearanceSettings from "./AppearanceSettings.svelte";
import { SettingsService } from "../../api/generated/index";
import { settings } from "../../stores/settings.svelte.js";
import { sync } from "../../stores/sync.svelte.js";
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
const originalIsDesktop = sync.isDesktop;

describe("AppearanceSettings", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    settingsService.putApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "agentsview",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
    });
    settings.chartPalette = "agentsview";
    settings.readOnly = false;
    settings.saving = false;
    Object.defineProperty(sync, "isDesktop", {
      value: false,
      writable: true,
      configurable: true,
    });
  });

  afterEach(() => {
    ui.applyZoomLevel(100);
    if (ui.highContrast) ui.toggleHighContrast();
    settings.chartPalette = "agentsview";
    settings.readOnly = false;
    Object.defineProperty(sync, "isDesktop", {
      value: originalIsDesktop,
      writable: true,
      configurable: true,
    });
    cleanup();
  });

  it.each([false, true])("shares one Zoom selector with desktop=%s", async (isDesktop) => {
    Object.defineProperty(sync, "isDesktop", { value: isDesktop, writable: true, configurable: true });
    ui.applyZoomLevel(120);
    const { getByTitle, getByRole, getAllByRole, queryByText } = render(AppearanceSettings);
    expect(getByTitle("Zoom").textContent).toContain("120%");
    expect(getByRole("button", { name: "Zoom 120%" })).toBeTruthy();
    expect(queryByText("Text size")).toBeNull();
    expect(queryByText("Desktop zoom")).toBeNull();
    await fireEvent.click(getByTitle("Zoom"));
    expect(getAllByRole("option").map((option) => option.textContent?.trim())).toEqual([
      "67%", "75%", "80%", "90%", "100%", "110%", "120%", "125%", "130%", "150%", "175%", "200%",
    ]);
    expect(getByRole("option", { name: "120%" }).getAttribute("aria-selected")).toBe("true");
    await fireEvent.mouseDown(getByRole("option", { name: "150%" }));
    await waitFor(() => expect(getByTitle("Zoom").textContent).toContain("150%"));
    expect(ui.zoomLevel).toBe(150);
    expect(localStorage.getItem("agentsview-zoom-level")).toBe("150");
    ui.zoomOut();
    await waitFor(() => expect(getByTitle("Zoom").textContent).toContain("130%"));
    ui.resetZoom();
    await waitFor(() => expect(getByTitle("Zoom").textContent).toContain("100%"));
  });

  it("keeps local Zoom available when server settings are read-only", async () => {
    settings.readOnly = true;
    const { getByRole, getByTitle } = render(AppearanceSettings);

    expect((getByRole("button", { name: "Zoom 100%" }) as HTMLButtonElement).disabled).toBe(false);
    await fireEvent.click(getByTitle("Zoom"));
    await fireEvent.mouseDown(getByRole("option", { name: "120%" }));
    await waitFor(() => expect(getByTitle("Zoom").textContent).toContain("120%"));
    expect(ui.zoomLevel).toBe(120);
    expect(settingsService.putApiV1Settings).not.toHaveBeenCalled();
  });

  it("disables Zoom while settings are saving", () => {
    settings.saving = true;
    const { getByRole } = render(AppearanceSettings);

    expect((getByRole("button", { name: "Zoom 100%" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("rejects an unlisted percentage and shows localized empty text", async () => {
    const { getByTitle, getByRole, getByText, queryAllByRole } = render(AppearanceSettings);
    await fireEvent.click(getByTitle("Zoom"));
    const input = getByRole("combobox", { name: "Zoom" });
    await fireEvent.input(input, { target: { value: "133" } });
    expect(getByText("No matches")).toBeTruthy();
    expect(queryAllByRole("option")).toHaveLength(0);
    await fireEvent.keyDown(input, { key: "Enter" });
    expect(ui.zoomLevel).toBe(100);
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
