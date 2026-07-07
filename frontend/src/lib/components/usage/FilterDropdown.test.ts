import {
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { flushSync, mount, tick, unmount } from "svelte";
import FilterDropdown from "./FilterDropdown.svelte";
import { BRANCH_LIST_SEP } from "../../branchFilters.js";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = "";
  vi.restoreAllMocks();
});

async function openDropdown() {
  const trigger = document.querySelector<HTMLButtonElement>(
    ".kit-filter-dropdown__btn",
  );
  trigger?.click();
  flushSync();
  await tick();
}

function rowStates(): Array<{ name: string; included: boolean }> {
  return Array.from(
    document.querySelectorAll<HTMLButtonElement>(".kit-filter-dropdown__item"),
  ).map((row) => ({
    name:
      row.querySelector(".kit-filter-dropdown__label")?.textContent ?? "",
    included: row.classList.contains("active"),
  }));
}

describe("FilterDropdown separators", () => {
  it("keeps comma-containing names intact with a custom separator", async () => {
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
    await openDropdown();

    expect(rowStates()).toEqual([
      { name: "proj/feat,one", included: false },
      { name: "proj/main", included: true },
    ]);
  });

  it("splits on commas by default", async () => {
    component = mount(FilterDropdown, {
      target: document.body,
      props: {
        label: "Project",
        items: [{ name: "alpha" }, { name: "beta" }],
        excludedCsv: "alpha,beta",
        onToggle: () => {},
      },
    });
    await openDropdown();

    expect(rowStates()).toEqual([
      { name: "alpha", included: false },
      { name: "beta", included: false },
    ]);
  });
});
