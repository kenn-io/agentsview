import { cleanup, fireEvent, render } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import TerminalSettings from "./TerminalSettings.svelte";
import { ConfigService } from "../../api/generated/index";
import { ApiError } from "../../api/runtime.js";
import { settings } from "../../stores/settings.svelte.js";

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    ConfigService: {
      postApiV1ConfigTerminal: vi.fn(),
    },
  };
});

const configService = ConfigService as unknown as {
  postApiV1ConfigTerminal: ReturnType<typeof vi.fn>;
};

describe("TerminalSettings", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    settings.terminal = { mode: "auto" };
    settings.saving = false;
  });

  afterEach(() => {
    cleanup();
  });

  it("shows why a refused save failed and keeps the edited choice", async () => {
    configService.postApiV1ConfigTerminal.mockRejectedValue(
      new ApiError(500, "save terminal config: config.toml is read-only"),
    );
    const load = vi.spyOn(settings, "load");
    const { getByRole, findByText } = render(TerminalSettings);

    await fireEvent.click(getByRole("radio", { name: "Clipboard only" }));
    await fireEvent.click(getByRole("button", { name: "Save" }));

    expect(await findByText("save terminal config: config.toml is read-only")).toBeTruthy();
    expect(getByRole("radio", { name: "Clipboard only" }).getAttribute("aria-checked")).toBe(
      "true",
    );
    expect(getByRole("button", { name: "Save" })).toBeTruthy();
    expect(load).not.toHaveBeenCalled();
  });
});
