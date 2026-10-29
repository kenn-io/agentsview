// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { flushSync, mount, unmount } from "svelte";
import { fireEvent, screen } from "@testing-library/svelte";
import SessionFilterControl from "./SessionFilterControl.svelte";
import SessionActiveFilters from "./SessionActiveFilters.svelte";
import { sessions, filtersToParams } from "../../stores/sessions.svelte.js";
import { ApiError } from "../../api/runtime.js";
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
  sessions.filters.minUserMessages = 0;
  sessions.filters.labels = [];
  sessions.filters.pr = "";
  vi.restoreAllMocks();
});

it("shows and searches machine labels while selecting the stored machine key", async () => {
  const response = {
    machines: ["installation-a", "historical-host", "source-a", "source-b", "source-c", "source-d"],
    machine_labels: { "installation-a": "Workstation A" },
    machine_aliases: {},
  };
  vi.spyOn(MetadataService, "getApiV1Machines").mockImplementation(async (params) =>
    params?.machine ? { ...response, resolved_machines: ["installation-a"] } : response,
  );
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

it("shows a temporary machine failure, disables rows, and retries when reopened", async () => {
  vi.spyOn(sessions, "loadAgents").mockResolvedValue();
  vi.spyOn(sessions, "load").mockResolvedValue();
  sessions.setMachineFilter("Retry label");
  let fail!: (reason: unknown) => void;
  vi.spyOn(MetadataService, "getApiV1Machines")
    .mockReturnValueOnce(
      new Promise((_, reject) => {
        fail = reject;
      }),
    )
    .mockResolvedValue({
      machines: ["retry-machine", "another-machine"],
      resolved_machines: ["retry-machine"],
      machine_labels: {},
      machine_aliases: {},
    });
  sessions.machines = ["retry-machine", "another-machine"];
  component = mount(SessionFilterControl, { target: document.body });
  await fireEvent.click(screen.getByRole("button", { name: "Filters" }));
  expect(screen.getByRole("status").textContent).toBe("Loading machines...");
  expect(
    (screen.getByRole("button", { name: "another-machine" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  fail(new ApiError(500, "internal error"));
  expect((await screen.findByRole("alert")).textContent).toContain(
    "Couldn't update the machine filter. Try again.",
  );
  expect(
    (screen.getByRole("button", { name: "another-machine" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  sessions.machines = [];
  flushSync();
  expect(screen.queryByText("No machines")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Filters" }));
  await fireEvent.click(screen.getByRole("button", { name: "Filters" }));
  await vi.waitFor(() =>
    expect(
      (screen.getByRole("button", { name: "another-machine" }) as HTMLButtonElement).disabled,
    ).toBe(false),
  );
  expect(screen.queryByRole("alert")).toBeNull();
});

it("selects hosted devices with the same display name by their distinct IDs", async () => {
  const devices = ["raw-device:61", "raw-device:62"];
  sessions.setSelectedMachines([]);
  sessions.machines = devices;
  sessions.machineLabels = { "raw-device:61": "Workstation", "raw-device:62": "Workstation" };
  vi.spyOn(sessions, "loadAgents").mockResolvedValue();
  vi.spyOn(sessions, "loadMachines").mockResolvedValue();
  vi.spyOn(sessions, "load").mockResolvedValue();

  component = mount(SessionFilterControl, { target: document.body });
  await fireEvent.click(screen.getByRole("button", { name: "Filters" }));

  expect(screen.getAllByRole("button", { name: "Workstation" })).toHaveLength(2);
  for (const device of devices) await fireEvent.click(screen.getByTitle(device));
  expect(sessions.selectedMachines).toEqual(devices);
  expect(filtersToParams(sessions.filters).machine).toBe(devices.join(","));
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
