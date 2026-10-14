import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { flushSync, mount, unmount } from "svelte";
import SessionActiveFilters from "./SessionActiveFilters.svelte";
import { sessions } from "../../stores/sessions.svelte.js";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = "";
  sessions.filters.project = "";
  sessions.filters.labels = [];
  sessions.filters.pr = "";
  vi.restoreAllMocks();
});

describe("SessionActiveFilters clear all", () => {
  it("clears usage filters while preserving hidden session filters", () => {
    vi.spyOn(sessions, "load").mockResolvedValue();
    sessions.filters.project = "alpha";
    sessions.filters.labels = ["ticket=example"];
    sessions.filters.pr = "acme/widgets#42";
    const onClearProjects = vi.fn();

    component = mount(SessionActiveFilters, {
      target: document.body,
      props: { onClearProjects },
    });
    flushSync();

    const clearAll = document.querySelector<HTMLButtonElement>(".clear-all");
    expect(clearAll).not.toBeNull();
    clearAll?.click();
    flushSync();

    expect(onClearProjects).toHaveBeenCalledOnce();
    expect(sessions.filters.project).toBe("");
    expect(sessions.filters.labels).toEqual(["ticket=example"]);
    expect(sessions.filters.pr).toBe("acme/widgets#42");
  });
});
