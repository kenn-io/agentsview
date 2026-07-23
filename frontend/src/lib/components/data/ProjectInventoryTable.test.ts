// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
import { fireEvent, screen } from "@testing-library/svelte";
import ProjectInventoryTable from "./ProjectInventoryTable.svelte";
import { m } from "../../i18n/index.js";
import type { DbProjectInventory, DbProjectInventoryRow } from "../../api/generated/index";

function makeRow(overrides: Partial<DbProjectInventoryRow> = {}): DbProjectInventoryRow {
  return {
    agents: 1,
    distinct_cwds: 1,
    enabled_rules_targeting: 0,
    label: "Project",
    machines: 1,
    project_key: "k1",
    recorded_as_original: false,
    sessions: 1,
    ...overrides,
  };
}

function fixtureRows(): DbProjectInventoryRow[] {
  return [
    makeRow({
      project_key: "alpha",
      label: "Alpha",
      sessions: 5,
      machines: 1,
      agents: 1,
      distinct_cwds: 1,
      first_activity: "2026-06-01T00:00:00Z",
      last_activity: "2026-06-02T00:00:00Z",
    }),
    makeRow({
      project_key: "beta",
      label: "Beta",
      sessions: 20,
      machines: 3,
      agents: 2,
      distinct_cwds: 4,
      first_activity: undefined,
      last_activity: undefined,
      enabled_rules_targeting: 2,
      recorded_as_original: true,
    }),
    makeRow({
      project_key: "gamma",
      label: "Gamma",
      sessions: 10,
      machines: 2,
      agents: 1,
      distinct_cwds: 2,
      first_activity: "2026-05-01T00:00:00Z",
      last_activity: "2026-05-15T00:00:00Z",
    }),
  ];
}

function makeInventory(rows: DbProjectInventoryRow[]): DbProjectInventory {
  return {
    governed_sessions: rows.reduce((sum, r) => sum + (r.enabled_rules_targeting > 0 ? r.sessions : 0), 0),
    projects: rows,
    total_projects: rows.length,
    total_sessions: rows.reduce((sum, r) => sum + r.sessions, 0),
  };
}

function rowOrder(): string[] {
  return [...document.querySelectorAll(".project-row")].map(
    (el) => el.getAttribute("data-project-key") ?? "",
  );
}

describe("ProjectInventoryTable", () => {
  let component: ReturnType<typeof mount> | undefined;

  afterEach(() => {
    if (component) unmount(component);
    component = undefined;
    document.body.innerHTML = "";
  });

  it("renders one row per project, default-sorted by sessions descending", async () => {
    const inventory = makeInventory(fixtureRows());
    component = mount(ProjectInventoryTable, {
      target: document.body,
      props: { inventory, selectedKey: "", onSelect: () => {} },
    });
    await tick();

    expect(rowOrder()).toEqual(["beta", "gamma", "alpha"]);
  });

  it("re-sorts when a column header is clicked", async () => {
    const inventory = makeInventory(fixtureRows());
    component = mount(ProjectInventoryTable, {
      target: document.body,
      props: { inventory, selectedKey: "", onSelect: () => {} },
    });
    await tick();

    const projectHeader = screen.getByRole("button", { name: m.data_col_project() });
    await fireEvent.click(projectHeader);
    await tick();

    // label-asc: Alpha, Beta, Gamma.
    expect(rowOrder()).toEqual(["alpha", "beta", "gamma"]);
  });

  it("filters rows by label substring, case-insensitively", async () => {
    const inventory = makeInventory(fixtureRows());
    component = mount(ProjectInventoryTable, {
      target: document.body,
      props: { inventory, selectedKey: "", onSelect: () => {} },
    });
    await tick();

    const filterInput = screen.getByRole("textbox", { name: m.data_filter_projects() });
    await fireEvent.input(filterInput, { target: { value: "alp" } });
    await tick();

    expect(rowOrder()).toEqual(["alpha"]);

    await fireEvent.input(filterInput, { target: { value: "nonexistent" } });
    await tick();

    expect(rowOrder()).toEqual([]);
    expect(document.body.textContent).toContain(m.data_no_matches());
  });

  it("renders an em dash for null first/last activity", async () => {
    const inventory = makeInventory(fixtureRows());
    component = mount(ProjectInventoryTable, {
      target: document.body,
      props: { inventory, selectedKey: "", onSelect: () => {} },
    });
    await tick();

    const betaRow = document.querySelector('.project-row[data-project-key="beta"]');
    const cells = betaRow?.querySelectorAll("td");
    expect(cells?.[5]?.textContent?.trim()).toBe("—");
    expect(cells?.[6]?.textContent?.trim()).toBe("—");
  });

  it("renders rule annotations with accessible titles", async () => {
    const inventory = makeInventory(fixtureRows());
    component = mount(ProjectInventoryTable, {
      target: document.body,
      props: { inventory, selectedKey: "", onSelect: () => {} },
    });
    await tick();

    const betaRow = document.querySelector('.project-row[data-project-key="beta"]');
    const ruleBadge = betaRow?.querySelector(".rule-badge");
    expect(ruleBadge?.textContent?.trim()).toBe("2");
    expect(ruleBadge?.getAttribute("title")).toBe(m.data_rules_targeting({ count: 2 }));

    const originalBadge = betaRow?.querySelector(".original-badge");
    expect(originalBadge).toBeTruthy();
    expect(originalBadge?.getAttribute("title")).toBe(m.data_recorded_original());

    const alphaRow = document.querySelector('.project-row[data-project-key="alpha"]');
    expect(alphaRow?.querySelector(".rule-badge")).toBeNull();
    expect(alphaRow?.querySelector(".original-badge")).toBeNull();
  });

  it("calls onSelect with the clicked row's project_key and marks the selected row", async () => {
    const onSelect = vi.fn();
    const inventory = makeInventory(fixtureRows());
    component = mount(ProjectInventoryTable, {
      target: document.body,
      props: { inventory, selectedKey: "gamma", onSelect },
    });
    await tick();

    const gammaRow = document.querySelector('.project-row[data-project-key="gamma"]');
    expect(gammaRow?.getAttribute("aria-selected")).toBe("true");
    expect(gammaRow?.classList.contains("selected")).toBe(true);

    const betaRow = document.querySelector('.project-row[data-project-key="beta"]');
    expect(betaRow?.getAttribute("aria-selected")).toBe("false");

    await fireEvent.click(betaRow as Element);
    expect(onSelect).toHaveBeenCalledWith("beta");
  });

  it("renders a localized fallback for empty labels that stays filterable and selectable", async () => {
    const onSelect = vi.fn();
    const inventory = makeInventory([
      makeRow({ project_key: "blank", label: "", sessions: 1 }),
      makeRow({ project_key: "alpha", label: "Alpha", sessions: 5 }),
    ]);
    component = mount(ProjectInventoryTable, {
      target: document.body,
      props: { inventory, selectedKey: "", onSelect },
    });
    await tick();

    const blankRow = document.querySelector('.project-row[data-project-key="blank"]');
    expect(blankRow?.querySelector(".label-text")?.textContent).toBe(m.shared_unknown());
    expect(blankRow?.querySelector(".col-project")?.getAttribute("title")).toBe(
      m.shared_unknown(),
    );

    const filterInput = screen.getByRole("textbox", { name: m.data_filter_projects() });
    await fireEvent.input(filterInput, { target: { value: m.shared_unknown() } });
    await tick();

    expect(rowOrder()).toEqual(["blank"]);

    await fireEvent.click(
      document.querySelector('.project-row[data-project-key="blank"]') as Element,
    );
    expect(onSelect).toHaveBeenCalledWith("blank");
  });

  it("is keyboard-activatable via Enter and Space", async () => {
    const onSelect = vi.fn();
    const inventory = makeInventory(fixtureRows());
    component = mount(ProjectInventoryTable, {
      target: document.body,
      props: { inventory, selectedKey: "", onSelect },
    });
    await tick();

    const betaRow = document.querySelector('.project-row[data-project-key="beta"]') as HTMLElement;
    await fireEvent.keyDown(betaRow, { key: "Enter" });
    expect(onSelect).toHaveBeenCalledWith("beta");

    onSelect.mockClear();
    const alphaRow = document.querySelector('.project-row[data-project-key="alpha"]') as HTMLElement;
    await fireEvent.keyDown(alphaRow, { key: " " });
    expect(onSelect).toHaveBeenCalledWith("alpha");
  });
});
