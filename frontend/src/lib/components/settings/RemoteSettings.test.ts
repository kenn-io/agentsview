// @vitest-environment jsdom
import { cleanup, fireEvent, render, waitFor } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
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
  settings.restartSupported = true;
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
  settings.requireAuth = false;
  settings.authToken = "";
});

describe("RemoteSettings", () => {
  it.each([27, 28])("checks API %i before connecting", async (apiVersion) => {
    const fetch = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ api_version: apiVersion, version: "test" }), {
        headers: { "Content-Type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetch);
    const { getByRole, getByLabelText, queryByText } = render(RemoteSettings);
    await fireEvent.input(getByLabelText("Server URL"), {
      target: { value: "https://remote.example.test" },
    });
    await fireEvent.input(getByLabelText("Auth token"), { target: { value: "test-token" } });
    await fireEvent.click(getByRole("button", { name: "Connect", exact: true }));
    if (apiVersion === 27) {
      await waitFor(() =>
        expect(queryByText("Upgrade the remote AgentsView server to continue.")).not.toBeNull(),
      );
      expect(localStorage.getItem("agentsview-server-url")).toBeNull();
    } else {
      await waitFor(() =>
        expect(localStorage.getItem("agentsview-server-url")).toBe("https://remote.example.test"),
      );
      expect(localStorage.getItem("agentsview-auth-token::https://remote.example.test")).toBe(
        "test-token",
      );
    }
    expect(fetch).toHaveBeenCalledOnce();
    expect(fetch.mock.calls[0]![0]).toBe("https://remote.example.test/api/v1/version");
  });

  it("offers a restart beside the auth notice before any save", async () => {
    const restart = vi.spyOn(settings, "restartDaemon").mockResolvedValue();
    const { getByRole } = render(RemoteSettings);

    expect(getByRole("status").textContent).toContain("Auth changes take effect immediately.");
    await fireEvent.click(getByRole("button", { name: "Restart now" }));

    expect(restart).toHaveBeenCalledOnce();
  });

  it("shows only the auth notice when the server can't restart itself", () => {
    settings.restartSupported = false;
    const { getByRole, queryByRole } = render(RemoteSettings);

    expect(getByRole("status").textContent?.trim()).toBe("Auth changes take effect immediately.");
    expect(queryByRole("button", { name: /Restart/ })).toBeNull();
  });

  it("shows the requested authentication state while saving", async () => {
    let releaseSave!: () => void;
    const saveSpy = vi.spyOn(settings, "save").mockImplementation(async (patch) => {
      expect(patch).toEqual({ require_auth: true });
      await new Promise<void>((resolve) => {
        releaseSave = resolve;
      });
      settings.requireAuth = true;
      return true;
    });
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
    const saveSpy = vi.spyOn(settings, "save").mockImplementation(async (patch) => {
      expect(patch).toEqual({ require_auth: true });
      return false;
    });
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
