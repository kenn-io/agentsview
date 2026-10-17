// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { mount, unmount } from "svelte";
import { fireEvent, screen } from "@testing-library/svelte";
import ActiveFilters from "./ActiveFilters.svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { MetadataService } from "../../api/generated/index.js";

let component: ReturnType<typeof mount> | undefined;
afterEach(() => {
  if (component) unmount(component);
  document.body.innerHTML = "";
  analytics.machine = "";
  sessions.filters.machine = "";
  vi.restoreAllMocks();
});

it("keeps an unresolved comma-containing machine as one removable chip", async () => {
  vi.spyOn(sessions, "load").mockResolvedValue();
  vi.spyOn(analytics, "fetchAll").mockResolvedValue();
  analytics.machine = "Work, Linux";
  sessions.filters.machine = "Work, Linux";
  component = mount(ActiveFilters, { target: document.body });
  const chip = screen.getByRole("button", { name: "Work, Linux" });
  await fireEvent.click(chip);
  expect(analytics.machine).toBe("");
  expect(sessions.filters.machine).toBe("");
});

it("keeps separate machine chips and picker choices after removing one of three machines", async () => {
  vi.spyOn(sessions, "load").mockResolvedValue();
  vi.spyOn(analytics, "fetchAll").mockResolvedValue();
  vi.spyOn(MetadataService, "getApiV1Machines").mockResolvedValue({
    machines: ["a", "b", "c"],
    machine_labels: {},
    machine_aliases: {},
    resolved_machines: ["a", "b", "c"],
  });
  analytics.machine = "a,b,c";
  sessions.filters.machine = "a,b,c";
  await sessions.loadMachines();
  component = mount(ActiveFilters, { target: document.body });

  await fireEvent.click(screen.getByRole("button", { name: "c" }));

  expect(analytics.machine).toBe("a,b");
  expect(sessions.filters.machine).toBe("a,b");
  expect(screen.getByRole("button", { name: "a" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "b" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "c" })).toBeNull();
});
