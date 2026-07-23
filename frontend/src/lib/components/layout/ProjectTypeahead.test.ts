// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, screen } from "@testing-library/svelte";
import { mount, unmount } from "svelte";
import type { ProjectInfo } from "../../api/types/core.js";
import { m } from "../../i18n/index.js";
import ProjectTypeahead from "./ProjectTypeahead.svelte";

const projects: ProjectInfo[] = [
  { name: "", session_count: 53 },
  { name: "repo-a", session_count: 2 },
];

describe("ProjectTypeahead", () => {
  let component: Record<string, unknown> | null = null;

  afterEach(() => {
    if (component) {
      unmount(component);
      component = null;
    }
    document.body.innerHTML = "";
  });

  it("opens without crashing when a project has an empty name", async () => {
    const onselect = vi.fn();
    component = mount(ProjectTypeahead, {
      target: document.body,
      props: { projects, value: "", onselect },
    });

    await fireEvent.click(screen.getByRole("button"));

    // The omitted empty-name row cannot round-trip through the "" = all-projects contract.
    const options = screen.getAllByRole("option");
    expect(options.map((option) => option.textContent?.trim())).toEqual([
      m.shared_all_projects(),
      "repo-a (2)",
    ]);

    await fireEvent.mouseDown(options[0]!);
    expect(onselect).toHaveBeenLastCalledWith("");

    await fireEvent.click(screen.getByRole("button"));
    await fireEvent.mouseDown(screen.getAllByRole("option")[1]!);
    expect(onselect).toHaveBeenLastCalledWith("repo-a");
  });

  it("keeps a nonempty external value selected", async () => {
    component = mount(ProjectTypeahead, {
      target: document.body,
      props: { projects, value: "repo-a", onselect: vi.fn() },
    });

    await fireEvent.click(screen.getByRole("button"));

    expect(screen.getByRole("option", { name: "repo-a (2)" }).getAttribute("aria-selected")).toBe(
      "true",
    );
    expect(
      screen.getByRole("option", { name: m.shared_all_projects() }).getAttribute("aria-selected"),
    ).toBe("false");
  });

  it("can omit the all-projects option for a required selection", async () => {
    component = mount(ProjectTypeahead, {
      target: document.body,
      props: {
        projects,
        value: "",
        onselect: vi.fn(),
        includeAll: false,
      },
    });

    await fireEvent.click(screen.getByRole("button"));

    expect(screen.queryByRole("option", { name: m.shared_all_projects() })).toBeNull();
    expect(screen.getByRole("option", { name: "repo-a (2)" })).toBeTruthy();
  });

  it("commits a nonempty custom project alongside partial matches", async () => {
    const onselect = vi.fn();
    component = mount(ProjectTypeahead, {
      target: document.body,
      props: {
        projects,
        value: "",
        onselect,
        includeAll: false,
        allowCustom: true,
        customLabel: 'Use project "{query}"',
      },
    });

    await fireEvent.click(screen.getByRole("button"));
    const input = screen.getByRole("combobox");
    await fireEvent.input(input, { target: { value: "repo" } });

    expect(screen.getByRole("option", { name: "repo-a (2)" })).toBeTruthy();
    const custom = screen.getByRole("option", { name: 'Use project "repo"' });
    await fireEvent.mouseDown(custom);
    expect(onselect).toHaveBeenCalledWith("repo");

    await fireEvent.click(screen.getByRole("button"));
    await fireEvent.input(screen.getByRole("combobox"), { target: { value: "   " } });
    expect(screen.queryByRole("option", { name: 'Use project ""' })).toBeNull();
    expect(screen.queryByRole("option", { name: /^Use project/ })).toBeNull();
  });

  it("reports query edits to callers that need to invalidate derived state", async () => {
    const onquery = vi.fn();
    component = mount(ProjectTypeahead, {
      target: document.body,
      props: {
        projects,
        value: "repo-a",
        onselect: vi.fn(),
        onquery,
      },
    });

    await fireEvent.click(screen.getByRole("button"));
    await fireEvent.input(screen.getByRole("combobox"), { target: { value: "repo-b" } });

    expect(onquery).toHaveBeenLastCalledWith("repo-b");
  });
});
