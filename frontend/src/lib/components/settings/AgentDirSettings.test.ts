// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import AgentDirSettings from "./AgentDirSettings.svelte";
import { settings } from "../../stores/settings.svelte.js";
import { initI18n } from "../../i18n/index.js";

function providerSwitch(name: string): HTMLInputElement {
  const control = document.body.querySelector<HTMLInputElement>(
    `input[role="switch"][aria-label="${name}"]`,
  );
  if (!control) throw new Error(`missing provider switch: ${name}`);
  return control;
}

beforeEach(() => {
  vi.restoreAllMocks();
  localStorage.clear();
  initI18n();
  settings.sessionProviders = [
    {
      id: "claude",
      display_name: "Claude Code",
      dirs: ["/sessions/claude"],
    },
    {
      id: "gemini",
      display_name: "Gemini",
      dirs: ["/sessions/gemini"],
    },
  ];
  settings.disabledAgents = ["gemini"];
  settings.readOnly = false;
  settings.saving = false;
  settings.saveError = null;
});

afterEach(() => {
  document.body.innerHTML = "";
});

describe("AgentDirSettings", () => {
  it("enables a disabled provider and shows restart guidance", async () => {
    const save = vi.spyOn(settings, "save").mockImplementation(async (patch) => {
      settings.disabledAgents = patch.disabled_agents ?? [];
      return true;
    });
    const component = mount(AgentDirSettings, { target: document.body });
    await tick();

    const gemini = providerSwitch("Enable Gemini session sync");
    expect(gemini.checked).toBe(false);
    gemini.click();
    await tick();
    await tick();

    expect(save).toHaveBeenCalledWith({ disabled_agents: [] });
    expect(providerSwitch("Enable Gemini session sync").checked).toBe(true);
    expect(document.body.querySelector('[role="status"]')?.textContent).toContain("Restart");
    unmount(component);
  });

  it("rolls back a rejected change and disables every row while saving", async () => {
    let finish!: (saved: boolean) => void;
    const save = vi.spyOn(settings, "save").mockReturnValue(
      new Promise((resolve) => {
        finish = resolve;
      }),
    );
    const component = mount(AgentDirSettings, { target: document.body });
    await tick();

    providerSwitch("Enable Claude Code session sync").click();
    await tick();
    expect(save).toHaveBeenCalledWith({
      disabled_agents: ["gemini", "claude"],
    });
    expect(providerSwitch("Enable Claude Code session sync").disabled).toBe(true);
    expect(providerSwitch("Enable Gemini session sync").disabled).toBe(true);

    finish(false);
    await tick();
    await tick();

    expect(providerSwitch("Enable Claude Code session sync").checked).toBe(true);
    expect(document.body.querySelector('[role="alert"]')?.textContent).toContain(
      "previous selection",
    );
    unmount(component);
  });

  it("disables provider switches in read-only mode", async () => {
    settings.readOnly = true;
    const component = mount(AgentDirSettings, { target: document.body });
    await tick();

    expect(providerSwitch("Enable Claude Code session sync").disabled).toBe(true);
    expect(providerSwitch("Enable Gemini session sync").disabled).toBe(true);
    unmount(component);
  });

  it("disables provider switches while another settings save is active", async () => {
    settings.saving = true;
    const component = mount(AgentDirSettings, { target: document.body });
    await tick();

    expect(providerSwitch("Enable Claude Code session sync").disabled).toBe(true);
    expect(providerSwitch("Enable Gemini session sync").disabled).toBe(true);
    unmount(component);
  });
});
