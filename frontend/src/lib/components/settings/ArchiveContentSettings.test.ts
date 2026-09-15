import { cleanup, fireEvent, render } from "@testing-library/svelte";
import { tick } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import ArchiveContentSettings from "./ArchiveContentSettings.svelte";
import { SettingsService } from "../../api/generated/index";
import { settings } from "../../stores/settings.svelte.js";

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

function makeSettingsResponse(toolResultImages: "keep" | "drop" | "offload") {
  return {
    agent_dirs: {},
    chart_palette: "agentsview",
    tool_result_images: toolResultImages,
    github_configured: false,
    host: "127.0.0.1",
    port: 8080,
    read_only: false,
    require_auth: false,
    terminal: { mode: "auto" },
  };
}

describe("ArchiveContentSettings", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    settings.toolResultImages = "keep";
    settings.readOnly = false;
    settings.saving = false;
  });

  afterEach(() => {
    settings.toolResultImages = "keep";
    settings.readOnly = false;
    cleanup();
  });

  it("renders Keep, Drop, and Offload options with the store value marked aria-checked", () => {
    const { getByRole } = render(ArchiveContentSettings);
    expect(getByRole("radio", { name: "Keep" }).getAttribute("aria-checked")).toBe("true");
    expect(getByRole("radio", { name: "Drop" }).getAttribute("aria-checked")).toBe("false");
  });

  it("calls putApiV1Settings with tool_result_images drop when Drop is clicked", async () => {
    settingsService.putApiV1Settings.mockResolvedValue(makeSettingsResponse("drop"));
    const { getByRole } = render(ArchiveContentSettings);

    await fireEvent.click(getByRole("radio", { name: "Drop" }));

    expect(settingsService.putApiV1Settings).toHaveBeenCalledWith({
      tool_result_images: "drop",
    });
    expect(getByRole("radio", { name: "Drop" }).getAttribute("aria-checked")).toBe("true");
  });

  it("saves offload and retains the returned policy", async () => {
    settingsService.putApiV1Settings.mockResolvedValue(makeSettingsResponse("offload"));
    const { getByRole } = render(ArchiveContentSettings);
    await fireEvent.click(getByRole("radio", { name: "Offload" }));
    expect(settingsService.putApiV1Settings).toHaveBeenCalledWith({
      tool_result_images: "offload",
    });
    expect(getByRole("radio", { name: "Offload" }).getAttribute("aria-checked")).toBe("true");
  });

  it("shows no restart notice on first render and shows it after a saved selection", async () => {
    settingsService.putApiV1Settings.mockResolvedValue(makeSettingsResponse("drop"));
    const { getByRole, findByRole, queryByRole } = render(ArchiveContentSettings);

    expect(queryByRole("status")).toBeNull();

    await fireEvent.click(getByRole("radio", { name: "Drop" }));

    expect(await findByRole("status")).toBeTruthy();
  });

  it("shows no restart notice when the save fails", async () => {
    settingsService.putApiV1Settings.mockRejectedValue(new Error("write refused"));
    const { getByRole, queryByRole } = render(ArchiveContentSettings);

    await fireEvent.click(getByRole("radio", { name: "Drop" }));
    await vi.waitFor(() => {
      expect(settingsService.putApiV1Settings).toHaveBeenCalled();
    });
    await tick();

    expect(queryByRole("status")).toBeNull();
    // The rejected write leaves the stored policy alone, so the control stays
    // on the value the daemon still holds.
    expect(getByRole("radio", { name: "Keep" }).getAttribute("aria-checked")).toBe("true");
  });

  it("disables all radios when settings are read-only", () => {
    settings.readOnly = true;
    const { getByRole } = render(ArchiveContentSettings);

    expect((getByRole("radio", { name: "Keep" }) as HTMLButtonElement).disabled).toBe(true);
    expect((getByRole("radio", { name: "Drop" }) as HTMLButtonElement).disabled).toBe(true);
    expect((getByRole("radio", { name: "Offload" }) as HTMLButtonElement).disabled).toBe(true);
  });
});
