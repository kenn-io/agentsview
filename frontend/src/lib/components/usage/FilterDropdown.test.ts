import {
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { flushSync, mount, unmount } from "svelte";
import FilterDropdown from "./FilterDropdown.svelte";
import { BRANCH_LIST_SEP } from "../../branchFilters.js";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = "";
  vi.restoreAllMocks();
});

function openDropdown() {
  const trigger =
    document.querySelector<HTMLButtonElement>(".filter-trigger");
  trigger?.click();
  flushSync();
}

function rowStates(): Array<{ name: string; selected: boolean }> {
  return Array.from(
    document.querySelectorAll<HTMLButtonElement>(".dropdown-row"),
  ).map((row) => ({
    name: row.querySelector(".item-name")?.textContent ?? "",
    selected: row.classList.contains("selected"),
  }));
}

describe("FilterDropdown separators", () => {
  it("keeps comma-containing names intact with a custom separator", () => {
    const tokenA = "proj\u001ffeat,one";
    const tokenB = "proj\u001fmain";
    component = mount(FilterDropdown, {
      target: document.body,
      props: {
        label: "Branch",
        items: [
          { name: tokenA, label: "proj/feat,one" },
          { name: tokenB, label: "proj/main" },
        ],
        excludedCsv: tokenA,
        separator: BRANCH_LIST_SEP,
        onToggle: () => {},
      },
    });
    openDropdown();

    expect(rowStates()).toEqual([
      { name: "proj/feat,one", selected: false },
      { name: "proj/main", selected: true },
    ]);
  });

  it("splits on commas by default", () => {
    component = mount(FilterDropdown, {
      target: document.body,
      props: {
        label: "Project",
        items: [{ name: "alpha" }, { name: "beta" }],
        excludedCsv: "alpha,beta",
        onToggle: () => {},
      },
    });
    openDropdown();

    expect(rowStates()).toEqual([
      { name: "alpha", selected: false },
      { name: "beta", selected: false },
    ]);
  });
});
