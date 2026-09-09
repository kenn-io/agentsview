// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
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
      homes_supported: true,
      homes: ["~/.claude-work"],
    },
    {
      id: "gemini",
      display_name: "Gemini",
      dirs: ["/sessions/gemini"],
      homes_supported: false,
      homes: [],
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
    await unmount(component);
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
    await unmount(component);
  });

  it("disables provider switches in read-only mode", async () => {
    settings.readOnly = true;
    const component = mount(AgentDirSettings, { target: document.body });
    await tick();

    expect(providerSwitch("Enable Claude Code session sync").disabled).toBe(true);
    expect(providerSwitch("Enable Gemini session sync").disabled).toBe(true);
    await unmount(component);
  });

  it("disables provider switches while another settings save is active", async () => {
    settings.saving = true;
    const component = mount(AgentDirSettings, { target: document.body });
    await tick();

    expect(providerSwitch("Enable Claude Code session sync").disabled).toBe(true);
    expect(providerSwitch("Enable Gemini session sync").disabled).toBe(true);
    await unmount(component);
  });

  it.each([
    {
      id: "claude",
      name: "Claude Code",
      env: "CLAUDE_CONFIG_DIR",
      first: "~/.claude-work",
      second: "~/.claude-personal",
    },
    {
      id: "codex",
      name: "Codex",
      env: "CODEX_HOME",
      first: "~/.codex-work",
      second: "~/.codex-personal",
    },
    {
      id: "pi",
      name: "Pi",
      env: "PI_CODING_AGENT_DIR",
      first: "~/.pi-work/agent",
      second: "~/.pi-personal/agent",
    },
  ])("adds and removes alternate homes for $name", async ({ id, name, env, first, second }) => {
    settings.sessionProviders = [
      { id, display_name: name, dirs: [], homes_supported: true, homes: [first] },
    ];
    const save = vi.spyOn(settings, "save").mockImplementation(async (patch) => {
      const homes = patch.agent_homes?.[id] ?? [];
      settings.sessionProviders = settings.sessionProviders.map((p) =>
        p.id === id ? { ...p, homes } : p,
      );
      return true;
    });
    const component = mount(AgentDirSettings, { target: document.body });
    await tick();

    expect(document.body.querySelector(".provider-homes-hint")?.textContent).toContain(env);
    const input = document.body.querySelector<HTMLInputElement>(
      `input[aria-label="New ${name} home directory"]`,
    );
    if (!input) throw new Error("missing home input");
    input.value = ` ${second} `;
    input.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await tick();
    await tick();

    expect(save).toHaveBeenCalledWith({
      agent_homes: { [id]: [first, second] },
    });
    const homes = [...document.body.querySelectorAll("code")].map((el) => el.textContent);
    expect(homes).toContain(second);
    expect(document.body.querySelector('[role="status"]')?.textContent).toContain("Restart");
    const remove = document.body.querySelector<HTMLButtonElement>(
      `button[aria-label="Remove ${name} home ${second}"]`,
    );
    if (!remove) throw new Error("missing remove button");
    remove.click();
    await tick();
    await tick();
    expect(save).toHaveBeenLastCalledWith({ agent_homes: { [id]: [first] } });
    expect([...document.body.querySelectorAll("code")].map((el) => el.textContent)).toContain(
      first,
    );
    expect(
      document.body.querySelector(`button[aria-label="Remove ${name} home ${second}"]`),
    ).toBeNull();
    await unmount(component);
  });

  it("removes an alternate home", async () => {
    const save = vi.spyOn(settings, "save").mockImplementation(async (patch) => {
      const homes = patch.agent_homes?.claude ?? [];
      settings.sessionProviders = settings.sessionProviders.map((p) =>
        p.id === "claude" ? { ...p, homes } : p,
      );
      return true;
    });
    const component = mount(AgentDirSettings, { target: document.body });
    await tick();

    const remove = document.body.querySelector<HTMLButtonElement>(
      'button[aria-label="Remove Claude Code home ~/.claude-work"]',
    );
    if (!remove) throw new Error("missing remove button");
    remove.click();
    await tick();
    await tick();

    expect(save).toHaveBeenCalledWith({ agent_homes: { claude: [] } });
    expect(
      document.body.querySelector('button[aria-label="Remove Claude Code home ~/.claude-work"]'),
    ).toBeNull();
    await unmount(component);
  });
});
