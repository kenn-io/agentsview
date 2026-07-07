import {
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { flushSync, mount, unmount } from "svelte";
import SessionActiveFilters from "./SessionActiveFilters.svelte";
import { sessions } from "../../stores/sessions.svelte.js";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = "";
  sessions.filters.project = "";
  vi.restoreAllMocks();
});

describe("SessionActiveFilters clear all", () => {
  it("invokes every usage-level clear hook", () => {
    vi.spyOn(sessions, "clearSessionFilters").mockImplementation(
      () => {},
    );
    vi.spyOn(sessions, "load").mockResolvedValue();
    sessions.filters.project = "alpha";
    const onClearProjects = vi.fn();
    const onClearModels = vi.fn();
    const onClearAgents = vi.fn();

    component = mount(SessionActiveFilters, {
      target: document.body,
      props: { onClearProjects, onClearModels, onClearAgents },
    });
    flushSync();

    const clearAll =
      document.querySelector<HTMLButtonElement>(".clear-all");
    expect(clearAll).not.toBeNull();
    clearAll?.click();
    flushSync();

    expect(onClearProjects).toHaveBeenCalledOnce();
    expect(onClearModels).toHaveBeenCalledOnce();
    expect(onClearAgents).toHaveBeenCalledOnce();
  });
});
