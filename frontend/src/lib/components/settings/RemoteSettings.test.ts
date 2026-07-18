// @vitest-environment jsdom
import {
  cleanup,
  fireEvent,
  render,
  waitFor,
} from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { tick } from "svelte";

import { initI18n } from "../../i18n/index.js";
import { settings } from "../../stores/settings.svelte.js";
import RemoteSettings from "./RemoteSettings.svelte";

beforeEach(() => {
  localStorage.clear();
  initI18n();
  settings.requireAuth = false;
  settings.authToken = "";
  settings.host = "127.0.0.1";
  settings.port = 8080;
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  localStorage.clear();
  settings.requireAuth = false;
  settings.authToken = "";
});

describe("RemoteSettings", () => {
  it("shows the requested authentication state while saving", async () => {
    let releaseSave!: () => void;
    const saveSpy = vi.spyOn(settings, "save").mockImplementation(
      async (patch) => {
        expect(patch).toEqual({ require_auth: true });
        await new Promise<void>((resolve) => {
          releaseSave = resolve;
        });
        settings.requireAuth = true;
      },
    );
    const { getByRole } = render(RemoteSettings);
    const requireAuth = getByRole("switch", {
      name: "Require auth token",
    }) as HTMLInputElement;

    await fireEvent.click(requireAuth);
    await tick();

    try {
      expect(saveSpy).toHaveBeenCalledOnce();
      expect(requireAuth.checked).toBe(true);
      expect(requireAuth.disabled).toBe(true);
      expect(requireAuth.closest("label")?.textContent).toContain("Enabled");
    } finally {
      releaseSave();
      await saveSpy.mock.results[0]!.value;
      await tick();
    }
  });

  it("rolls back when saving leaves authentication unchanged", async () => {
    const saveSpy = vi.spyOn(settings, "save").mockImplementation(
      async (patch) => {
        expect(patch).toEqual({ require_auth: true });
      },
    );
    const { getByRole } = render(RemoteSettings);
    const requireAuth = getByRole("switch", {
      name: "Require auth token",
    }) as HTMLInputElement;

    await fireEvent.click(requireAuth);
    await waitFor(() => expect(requireAuth.disabled).toBe(false));

    expect(saveSpy).toHaveBeenCalledOnce();
    expect(settings.requireAuth).toBe(false);
    expect(requireAuth.checked).toBe(false);
    expect(requireAuth.closest("label")?.textContent).toContain("Disabled");
  });
});
