// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { flushSync, mount, unmount } from "svelte";
import { fireEvent, screen } from "@testing-library/svelte";
import SessionFilterControl from "./SessionFilterControl.svelte";
import SessionActiveFilters from "./SessionActiveFilters.svelte";
import { sessions, filtersToParams } from "../../stores/sessions.svelte.js";
import { MetadataService } from "../../api/generated/index.js";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = "";
  sessions.agents = [];
  sessions.filters.agent = "";
  sessions.machines = [];
  sessions.machineLabels = {};
  sessions.filters.machine = "";
  vi.restoreAllMocks();
});

it("shows and searches machine labels while selecting the stored machine key", async () => {
  const response = {
    machines: ["installation-a", "historical-host", "source-a", "source-b", "source-c", "source-d"],
    machine_labels: { "installation-a": "Workstation A" },
    machine_aliases: {},
  };
  vi.spyOn(MetadataService, "getApiV1Machines").mockResolvedValue(response);
  vi.spyOn(sessions, "loadAgents").mockResolvedValue();
  vi.spyOn(sessions, "load").mockResolvedValue();
  await sessions.loadMachines();

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
  it("keeps custom session labels under one base Claude option", () => {
    sessions.agents = [{ name: "claude", session_count: 2 }];
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "loadMachines").mockResolvedValue();

    component = mount(SessionFilterControl, { target: document.body });
    document.querySelector<HTMLButtonElement>(".filter-btn")?.click();
    flushSync();

    const rows = document.querySelectorAll(".agent-select-row");
    expect(rows).toHaveLength(2);
    expect(document.querySelectorAll(".agent-select-name")[1]?.textContent).toBe("Claude");

    (rows[1] as HTMLButtonElement).click();
    flushSync();
    expect(sessions.filters.agent).toBe("claude");
  });
});
