import { cleanup, fireEvent, render, waitFor } from "@testing-library/svelte";
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import NotificationsSettings from "./NotificationsSettings.svelte";
import { SettingsService, type SettingsResponse } from "../../api/generated/index";
import { settings } from "../../stores/settings.svelte.js";

vi.mock("../../api/generated/index", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/generated/index")>()),
  SettingsService: { putApiV1Settings: vi.fn() },
}));

beforeEach(() => {
  vi.clearAllMocks();
  settings.notifications = { enabled: false };
  settings.readOnly = false;
  settings.saving = false;
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it("saves the notification toggle and renders the saved value", async () => {
  const plugin = {
    sendNotification: vi.fn(),
  };
  vi.stubGlobal("__TAURI__", { notification: plugin });
  const save = vi.mocked(SettingsService.putApiV1Settings);
  const response: Omit<SettingsResponse, "notifications"> = {
    agent_dirs: {},
    chart_palette: "agentsview",
    github_configured: false,
    host: "127.0.0.1",
    port: 8080,
    read_only: false,
    require_auth: false,
    terminal: { mode: "auto" },
    disabled_agents: [],
    session_providers: [],
    tool_result_images: "keep",
    insight_default_agent: "claude",
  };
  save.mockResolvedValue({
    ...response,
    notifications: { enabled: false },
  });
  settings.notifications.enabled = true;
  const { getByRole, getByText, queryByRole } = render(NotificationsSettings);
  expect(
    getByText("Notifications follow your system's notification settings for AgentsView."),
  ).toBeTruthy();
  expect(queryByRole("status")).toBeNull();
  const enabled = () => getByRole("switch", { name: "Enable desktop notifications" });
  expect((enabled() as HTMLInputElement).checked).toBe(true);
  expect(save).not.toHaveBeenCalled();
  await fireEvent.click(enabled());
  await waitFor(() =>
    expect(save).toHaveBeenCalledWith({
      notifications: { enabled: false },
    }),
  );
  expect((enabled() as HTMLInputElement).checked).toBe(false);
  save.mockResolvedValue({
    ...response,
    notifications: { enabled: true },
  });
  await fireEvent.click(enabled());
  expect(save).toHaveBeenLastCalledWith({
    notifications: { enabled: true },
  });
  expect(plugin.sendNotification).not.toHaveBeenCalled();
});

it("reverts the toggle after a failed save", async () => {
  const name = "Enable desktop notifications";
  vi.stubGlobal("__TAURI__", { notification: { sendNotification: vi.fn() } });
  vi.mocked(SettingsService.putApiV1Settings).mockRejectedValueOnce(new Error("save failed"));
  const { getByRole } = render(NotificationsSettings);
  const toggle = getByRole("switch", { name }) as HTMLInputElement;
  await fireEvent.click(toggle);
  await waitFor(() => expect(toggle.checked).toBe(false));
});

it("explains an unavailable notification bridge", () => {
  const { getByRole } = render(NotificationsSettings);
  expect(getByRole("status").textContent).toBe(
    "Desktop notifications are unavailable in this app.",
  );
  expect(
    getByRole("switch", { name: "Enable desktop notifications" }).hasAttribute("disabled"),
  ).toBe(true);
});
