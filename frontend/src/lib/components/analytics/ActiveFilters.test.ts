// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { mount, unmount } from "svelte";
import { fireEvent, screen } from "@testing-library/svelte";
import ActiveFilters from "./ActiveFilters.svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";

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
