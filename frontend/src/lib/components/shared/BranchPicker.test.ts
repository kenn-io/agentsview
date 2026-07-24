// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { render } from "@testing-library/svelte";
import BranchPicker from "./BranchPicker.svelte";

type Deferred<T> = {
  promise: Promise<T>;
  resolve: (value: T) => void;
};

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

async function flush() {
  await tick();
  await Promise.resolve();
  await tick();
}

function rowNames(): string[] {
  return Array.from(
    document.querySelectorAll<HTMLButtonElement>("[role=option]:not(.all)"),
  ).map((row) => row.textContent?.trim() ?? "");
}

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) {
    unmount(component);
    component = undefined;
  }
  vi.useRealTimers();
  document.body.innerHTML = "";
});

describe("BranchPicker", () => {
  it("forwards project scope and limits ordinary results to 100", async () => {
    const search = vi.fn().mockResolvedValue({
      branches: Array.from({ length: 105 }, (_, i) => ({ branch: `branch-${i}` })),
      has_more: true,
    });
    component = mount(BranchPicker, {
      target: document.body,
      props: {
        mode: "multi",
        selected: [],
        projects: ["project-a"],
        search,
        label: "Branch",
        allLabel: "All branches",
        placeholder: "Search branches",
        clearSearchLabel: "Clear branch search",
        loadingLabel: "Loading branches…",
        emptyLabel: "No matching branches",
        refineLabel: "More branches exist. Refine your search.",
        onChange: () => {},
      },
    });

    document.querySelector<HTMLButtonElement>(".branch-picker-trigger")!.click();
    await flush();

    expect(search).toHaveBeenCalledWith({
      projects: ["project-a"],
      search: "",
      limit: 100,
    });
    expect(rowNames()).toHaveLength(100);
    expect(document.body.textContent).toContain(
      "More branches exist. Refine your search.",
    );
  });

  it("pins selected branches above search results without duplicating them", async () => {
    const search = vi.fn().mockResolvedValue({
      branches: [{ branch: "feature" }, { branch: "main" }],
      has_more: false,
    });
    component = mount(BranchPicker, {
      target: document.body,
      props: {
        mode: "multi",
        selected: ["main", "selected-only"],
        projects: [],
        search,
        label: "Branch",
        allLabel: "All branches",
        placeholder: "Search branches",
        clearSearchLabel: "Clear branch search",
        loadingLabel: "Loading branches…",
        emptyLabel: "No matching branches",
        refineLabel: "More branches exist. Refine your search.",
        onChange: () => {},
      },
    });

    document.querySelector<HTMLButtonElement>(".branch-picker-trigger")!.click();
    await flush();

    expect(search).toHaveBeenCalledWith({ search: "", limit: 100 });
    expect(rowNames()).toEqual(["main", "selected-only", "feature"]);
  });

  it("reloads with a changed project scope while open", async () => {
    const search = vi.fn().mockResolvedValue({ branches: [], has_more: false });
    const view = render(BranchPicker, {
      mode: "single",
      selected: [],
      projects: ["project-a"],
      search,
      label: "Branch",
      allLabel: "All branches",
      placeholder: "Search branches",
      clearSearchLabel: "Clear branch search",
      loadingLabel: "Loading branches…",
      emptyLabel: "No matching branches",
      refineLabel: "More branches exist. Refine your search.",
      onChange: () => {},
    });
    document.querySelector<HTMLButtonElement>(".branch-picker-trigger")!.click();
    await flush();

    await view.rerender({ projects: ["project-b"] });
    await flush();

    expect(search).toHaveBeenLastCalledWith({
      projects: ["project-b"],
      search: "",
      limit: 100,
    });
    view.unmount();
  });

  it("positions the panel fixed so overflow ancestors cannot clip it", async () => {
    const search = vi.fn().mockResolvedValue({ branches: [], has_more: false });
    component = mount(BranchPicker, {
      target: document.body,
      props: {
        mode: "single",
        selected: [],
        projects: [],
        search,
        label: "Branch",
        allLabel: "All branches",
        placeholder: "Search branches",
        clearSearchLabel: "Clear branch search",
        loadingLabel: "Loading branches…",
        emptyLabel: "No matching branches",
        refineLabel: "More branches exist. Refine your search.",
        onChange: () => {},
      },
    });
    const trigger = document.querySelector<HTMLButtonElement>(
      ".branch-picker-trigger",
    )!;
    vi.spyOn(trigger, "getBoundingClientRect").mockReturnValue({
      x: 20,
      y: 30,
      top: 30,
      right: 152,
      bottom: 58,
      left: 20,
      width: 132,
      height: 28,
      toJSON: () => ({}),
    });

    trigger.click();
    await flush();

    const panel = document.querySelector<HTMLElement>(".branch-picker-panel")!;
    const style = panel.getAttribute("style") ?? "";
    expect(style).toContain("left:");
    expect(style).toContain("top:");
  });

  it("waits for the debounce before searching typed text", async () => {
    vi.useFakeTimers();
    const search = vi.fn().mockResolvedValue({ branches: [], has_more: false });
    component = mount(BranchPicker, {
      target: document.body,
      props: {
        mode: "single",
        selected: [],
        projects: [],
        search,
        label: "Branch",
        allLabel: "All branches",
        placeholder: "Search branches",
        clearSearchLabel: "Clear branch search",
        loadingLabel: "Loading branches…",
        emptyLabel: "No matching branches",
        refineLabel: "More branches exist. Refine your search.",
        onChange: () => {},
      },
    });
    document.querySelector<HTMLButtonElement>(".branch-picker-trigger")!.click();
    await flush();

    const input = document.querySelector<HTMLInputElement>("input")!;
    input.value = "queued";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    await flush();

    expect(search).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(249);
    expect(search).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(search).toHaveBeenLastCalledWith({ search: "queued", limit: 100 });
  });

  it("cancels a queued search when the picker closes", async () => {
    vi.useFakeTimers();
    const search = vi.fn().mockResolvedValue({ branches: [], has_more: false });
    component = mount(BranchPicker, {
      target: document.body,
      props: {
        mode: "single",
        selected: [],
        projects: [],
        search,
        label: "Branch",
        allLabel: "All branches",
        placeholder: "Search branches",
        clearSearchLabel: "Clear branch search",
        loadingLabel: "Loading branches…",
        emptyLabel: "No matching branches",
        refineLabel: "More branches exist. Refine your search.",
        onChange: () => {},
      },
    });
    const trigger = document.querySelector<HTMLButtonElement>(".branch-picker-trigger")!;
    trigger.click();
    await flush();
    const input = document.querySelector<HTMLInputElement>("input")!;
    input.value = "queued";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    trigger.click();
    await vi.advanceTimersByTimeAsync(250);

    expect(search).toHaveBeenCalledTimes(1);
  });

  it("debounces search and suppresses stale responses", async () => {
    vi.useFakeTimers();
    const first = deferred<{ branches: { branch: string }[]; has_more: boolean }>();
    const second = deferred<{ branches: { branch: string }[]; has_more: boolean }>();
    const search = vi.fn()
      .mockResolvedValueOnce({ branches: [], has_more: false })
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(second.promise);
    component = mount(BranchPicker, {
      target: document.body,
      props: {
        mode: "single",
        selected: [],
        projects: ["project-a"],
        search,
        label: "Branch",
        allLabel: "All branches",
        placeholder: "Search branches",
        clearSearchLabel: "Clear branch search",
        loadingLabel: "Loading branches…",
        emptyLabel: "No matching branches",
        refineLabel: "More branches exist. Refine your search.",
        onChange: () => {},
      },
    });
    document.querySelector<HTMLButtonElement>(".branch-picker-trigger")!.click();
    await flush();

    const input = document.querySelector<HTMLInputElement>("input")!;
    input.value = "old";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    await vi.advanceTimersByTimeAsync(250);
    input.value = "new";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    await vi.advanceTimersByTimeAsync(250);

    second.resolve({ branches: [{ branch: "new-result" }], has_more: false });
    await flush();
    first.resolve({ branches: [{ branch: "old-result" }], has_more: false });
    await flush();

    expect(search).toHaveBeenLastCalledWith({
      projects: ["project-a"],
      search: "new",
      limit: 100,
    });
    expect(rowNames()).toEqual(["new-result"]);
  });
});
