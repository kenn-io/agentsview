// @vitest-environment jsdom
import { afterEach, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { makeProjects } from "../../test-support/analytics.js";
import ProjectBreakdown from "./ProjectBreakdown.svelte";

let component: ReturnType<typeof mount> | undefined;
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  analytics.projects = null;
  analytics.errors.projects = null;
  document.body.innerHTML = "";
});

it("distinguishes loading, accepted results, and failures with or without results", async () => {
  analytics.projects = null;
  analytics.errors.projects = null;
  component = mount(ProjectBreakdown, { target: document.body });
  await tick();
  expect(document.querySelector(".loading")).not.toBeNull();
  analytics.projects = makeProjects();
  await tick();
  expect(document.body.textContent).toContain("No project data");
  analytics.errors.projects = "request failed";
  await tick();
  expect(document.querySelector(".error")).not.toBeNull();
  expect(document.body.textContent).toContain("No project data");
  analytics.projects = null;
  await tick();
  expect(document.querySelector(".error")).not.toBeNull();
  expect(document.body.textContent).not.toContain("No project data");
});
