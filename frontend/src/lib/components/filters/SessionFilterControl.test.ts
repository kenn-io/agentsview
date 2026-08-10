// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import SessionFilterControl from "./SessionFilterControl.svelte";
import { sessions, parseFiltersFromParams } from "../../stores/sessions.svelte.js";
import { starred } from "../../stores/starred.svelte.js";
import { branchFilterToken } from "../../branchFilters.js";

let component: ReturnType<typeof mount> | undefined;

function resetSessionState() {
  sessions.filters = parseFiltersFromParams({});
  sessions.agents = [];
  sessions.machines = [];
  starred.filterOnly = false;
}

async function openDropdown() {
  component = mount(SessionFilterControl, { target: document.body });
  await tick();
  document
    .querySelector<HTMLButtonElement>(".filter-btn")!
    .click();
  await tick();
}

function sectionRowNames(label: string): string[] {
  const section = Array.from(
    document.querySelectorAll<HTMLDivElement>(".filter-section"),
  ).find(
    (s) =>
      s.querySelector(".filter-section-label")?.textContent?.trim() === label,
  );
  expect(section, `section ${label}`).toBeTruthy();
  return Array.from(
    section!.querySelectorAll<HTMLSpanElement>(".agent-select-name"),
  ).map((el) => el.textContent?.trim() ?? "");
}

beforeEach(() => {
  resetSessionState();
  vi.spyOn(sessions, "loadAgents").mockResolvedValue();
  vi.spyOn(sessions, "loadMachines").mockResolvedValue();
});

afterEach(() => {
  if (component) {
    unmount(component);
    component = undefined;
  }
  resetSessionState();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  document.body.innerHTML = "";
});

describe("SessionFilterControl agent options", () => {
  it("keeps custom session labels under one base Claude option", async () => {
    sessions.agents = [{ name: "claude", session_count: 2 }];
    await openDropdown();

    const rows = document.querySelectorAll(".agent-select-row");
    expect(rows).toHaveLength(2);
    expect(document.querySelectorAll(".agent-select-name")[1]?.textContent).toBe(
      "Claude",
    );

    (rows[1] as HTMLButtonElement).click();
    await tick();
    expect(sessions.filters.agent).toBe("claude");
  });
});

describe("SessionFilterControl filter badge", () => {
  it("shows no badge without active filters", async () => {
    await openDropdown();
    expect(document.querySelector(".filter-badge")).toBeNull();
  });

  it("shows the active filter count", async () => {
    sessions.filters.machine = "host-a";
    sessions.filters.branch = branchFilterToken("proj", "main");
    sessions.filters.includeAutomated = true;
    await openDropdown();

    const badge = document.querySelector(".filter-badge");
    expect(badge?.textContent).toBe("3");
  });
});

describe("SessionFilterControl selected-to-top sort", () => {
  it("floats the selected agent above higher-count agents", async () => {
    sessions.agents = [
      { name: "claude", session_count: 10 },
      { name: "codex", session_count: 2 },
    ];
    sessions.filters.agent = "codex";
    await openDropdown();

    const names = sectionRowNames("Agent");
    const codexIdx = names.findIndex((n) => n.includes("Codex"));
    const claudeIdx = names.findIndex((n) => n.includes("Claude"));
    expect(codexIdx).toBeGreaterThanOrEqual(0);
    expect(claudeIdx).toBeGreaterThanOrEqual(0);
    expect(codexIdx).toBeLessThan(claudeIdx);
  });

  it("floats the selected machine above alphabetical order", async () => {
    sessions.machines = ["alpha-host", "zeta-host"];
    sessions.filters.machine = "zeta-host";
    await openDropdown();

    const names = sectionRowNames("Machine");
    expect(names[0]).toBe("zeta-host");
    expect(names[1]).toBe("alpha-host");
  });

  it("passes the current project scope to the shared branch picker", async () => {
    sessions.filters.project = "proj-a";
    await openDropdown();

    const branchTrigger = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".branch-picker-trigger"),
    )[0]!;
    expect(branchTrigger).toBeTruthy();
    expect(branchTrigger.textContent).toContain("All Branches");
  });

  it("searches root-session branches for the sidebar", async () => {
    await openDropdown();
    expect(document.querySelector(".branch-picker-trigger")).toBeTruthy();
  });

  it("decodes a selected legacy project-pair branch for display", async () => {
    sessions.filters.branch = branchFilterToken("proj", "mid");
    await openDropdown();

    const branchTrigger = document.querySelector<HTMLButtonElement>(
      ".branch-picker-trigger",
    );
    expect(branchTrigger?.textContent).toContain("mid");
    expect(branchTrigger?.textContent).not.toContain("proj/");
  });
});
