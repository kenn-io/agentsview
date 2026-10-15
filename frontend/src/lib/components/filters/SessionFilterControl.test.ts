// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { flushSync, mount, tick, unmount } from "svelte";
import { fireEvent, screen } from "@testing-library/svelte";
import SessionFilterControl from "./SessionFilterControl.svelte";
import SessionActiveFilters from "./SessionActiveFilters.svelte";
import { sessions, filtersToParams, parseFiltersFromParams } from "../../stores/sessions.svelte.js";
import { starred } from "../../stores/starred.svelte.js";
import { MetadataService } from "../../api/generated/index.js";
import { branchFilterToken } from "../../branchFilters.js";

let component: ReturnType<typeof mount> | undefined;

function resetSessionState() {
  sessions.filters = parseFiltersFromParams({});
  sessions.agents = [];
  sessions.machines = [];
  sessions.machineLabels = {};
  starred.filterOnly = false;
}

async function openDropdown() {
  component = mount(SessionFilterControl, { target: document.body });
  await tick();
  document.querySelector<HTMLButtonElement>(".filter-btn")!.click();
  await tick();
}

function sectionRowNames(label: string): string[] {
  const section = Array.from(document.querySelectorAll<HTMLDivElement>(".filter-section")).find(
    (s) => s.querySelector(".filter-section-label")?.textContent?.trim() === label,
  );
  expect(section, `section ${label}`).toBeTruthy();
  return Array.from(section!.querySelectorAll<HTMLSpanElement>(".agent-select-name")).map(
    (el) => el.textContent?.trim() ?? "",
  );
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

it("shows and searches machine labels while selecting the stored machine key", async () => {
  const response = {
    machines: ["installation-a", "historical-host", "source-a", "source-b", "source-c", "source-d"],
    machine_labels: { "installation-a": "Workstation A" },
    machine_aliases: {},
  };
  vi.spyOn(MetadataService, "getApiV1Machines").mockResolvedValue(response);
  vi.spyOn(sessions, "load").mockResolvedValue();
  // The shared beforeEach stubs loadMachines; this test needs the real loader.
  vi.mocked(sessions.loadMachines).mockRestore();
  await sessions.loadMachines();
  vi.spyOn(sessions, "loadMachines").mockResolvedValue();

  component = mount(SessionFilterControl, { target: document.body });
  await fireEvent.click(screen.getByRole("button", { name: "Filters" }));

  expect(screen.getByRole("button", { name: "historical-host" })).toBeTruthy();
  await fireEvent.input(screen.getByPlaceholderText("Search machines..."), {
    target: { value: "workstation" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Workstation A" }));

  expect(sessions.filters.machine).toBe("installation-a");
  expect(filtersToParams(sessions.filters).machine).toBe("installation-a");

  await unmount(component);
  component = mount(SessionActiveFilters, { target: document.body });
  await fireEvent.click(screen.getByRole("button", { name: "Workstation A" }));
  expect(sessions.filters.machine).toBe("");
});

describe("SessionFilterControl agent options", () => {
  it("keeps custom session labels under one base Claude option", async () => {
    sessions.agents = [{ name: "claude", session_count: 2 }];
    await openDropdown();

    const rows = document.querySelectorAll(".agent-select-row");
    expect(rows).toHaveLength(2);
    expect(document.querySelectorAll(".agent-select-name")[1]?.textContent).toBe("Claude");

    (rows[1] as HTMLButtonElement).click();
    await tick();
    expect(sessions.filters.agent).toBe("claude");
  });
});

describe("SessionFilterControl minimum prompt filter", () => {
  async function openControl() {
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "loadMachines").mockResolvedValue();
    vi.spyOn(sessions, "load").mockResolvedValue();

    component = mount(SessionFilterControl, { target: document.body });
    await fireEvent.click(screen.getByRole("button", { name: "Filters" }));
  }

  it("turns the filter off when the active pill is clicked again", async () => {
    await openControl();

    await fireEvent.click(screen.getByRole("button", { name: "5" }));
    expect(sessions.filters.minUserMessages).toBe(5);
    expect(filtersToParams(sessions.filters).min_user_messages).toBe("5");

    await fireEvent.click(screen.getByRole("button", { name: "5" }));
    expect(sessions.filters.minUserMessages).toBe(0);
    expect(filtersToParams(sessions.filters).min_user_messages).toBeUndefined();
  });

  it("keeps the other filters when the minimum prompt filter is cleared", async () => {
    sessions.agents = [{ name: "claude", session_count: 2 }];
    await openControl();

    await fireEvent.click(screen.getByRole("button", { name: /Claude/ }));
    await fireEvent.click(screen.getByRole("button", { name: "5" }));
    expect(sessions.filters.agent).toBe("claude");
    expect(sessions.filters.minUserMessages).toBe(5);

    await fireEvent.click(screen.getByRole("button", { name: "5" }));
    expect(sessions.filters.minUserMessages).toBe(0);
    expect(sessions.filters.agent).toBe("claude");
    expect(filtersToParams(sessions.filters).agent).toBe("claude");
    expect(filtersToParams(sessions.filters).min_user_messages).toBeUndefined();
  });
});

describe("SessionFilterControl label and pull request filters", () => {
  async function openControl(props: Record<string, unknown> = {}) {
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "loadMachines").mockResolvedValue();
    vi.spyOn(sessions, "load").mockResolvedValue();

    component = mount(SessionFilterControl, { target: document.body, props });
    await fireEvent.click(screen.getByRole("button", { name: "Filters" }));
  }

  async function submit(input: HTMLElement, value: string) {
    await fireEvent.input(input, { target: { value } });
    await fireEvent.keyDown(input, { key: "Enter" });
  }

  it("adds labels on Enter and removes one from its row", async () => {
    await openControl();
    const input = screen.getByRole("textbox", { name: "Filter by label" });

    await submit(input, "ticket=example");
    await submit(input, "role=reviewer");
    expect(sessions.filters.labels).toEqual(["ticket=example", "role=reviewer"]);
    expect((input as HTMLInputElement).value).toBe("");

    await fireEvent.click(screen.getByRole("button", { name: "ticket=example" }));
    expect(sessions.filters.labels).toEqual(["role=reviewer"]);
  });

  it("applies a pull request filter and clears it from its row", async () => {
    await openControl();
    const input = screen.getByRole("textbox", { name: "Filter by pull request" });

    await submit(input, "acme/widgets#42");
    expect(sessions.filters.pr).toBe("acme/widgets#42");

    await fireEvent.click(screen.getByRole("button", { name: "acme/widgets#42" }));
    expect(sessions.filters.pr).toBe("");
  });

  it("preserves hidden label and pull request filters when shared filters clear", async () => {
    sessions.filters.labels = ["ticket=example"];
    sessions.filters.pr = "acme/widgets#42";
    await openControl({ showLabelFilters: false });

    expect(screen.queryByRole("textbox", { name: "Filter by label" })).toBeNull();
    expect(screen.queryByRole("textbox", { name: "Filter by pull request" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();

    sessions.filters.agent = "claude";
    flushSync();
    await fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(sessions.filters.agent).toBe("");
    expect(sessions.filters.labels).toEqual(["ticket=example"]);
    expect(sessions.filters.pr).toBe("acme/widgets#42");
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

  it("searches root-session branches within the current project", async () => {
    const searchBranches = vi
      .spyOn(MetadataService, "getApiV1BranchNames")
      .mockResolvedValue({ branches: [], has_more: false });
    sessions.filters.project = "proj-a";
    await openDropdown();

    document.querySelector<HTMLButtonElement>(".branch-picker-trigger")!.click();

    await vi.waitFor(() =>
      expect(searchBranches).toHaveBeenCalledWith(
        expect.objectContaining({
          projects: ["proj-a"],
          scope: "roots",
        }),
      ),
    );
  });

  it("decodes a selected legacy project-pair branch for display", async () => {
    sessions.filters.branch = branchFilterToken("proj", "mid");
    await openDropdown();

    const branchTrigger = document.querySelector<HTMLButtonElement>(".branch-picker-trigger");
    expect(branchTrigger?.textContent).toContain("mid");
    expect(branchTrigger?.textContent).not.toContain("proj/");
  });
});
