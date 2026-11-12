// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
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
  analytics.project = "";
  document.body.innerHTML = "";
  vi.restoreAllMocks();
});

function project(name: string, messages: number) {
  return {
    name,
    sessions: 2,
    messages,
    first_session: "",
    last_session: "",
    avg_messages: 0,
    median_messages: 0,
    agents: { claude: messages },
    daily_trend: 0,
  };
}

it("filters by a project from its row button and reports the filter state", async () => {
  vi.spyOn(analytics, "fetchAll").mockResolvedValue(undefined);
  analytics.projects = { projects: [project("alpha", 30), project("beta", 10)] };
  component = mount(ProjectBreakdown, { target: document.body });
  await tick();

  const rows = [...document.querySelectorAll<HTMLButtonElement>("button.bar-row")];
  expect(rows).toHaveLength(2);
  // The name carries the hover details, so screen readers get them too.
  expect(rows[0]!.textContent).toContain("alpha, 30 messages, 2 sessions, claude: 30");
  expect(rows[0]!.getAttribute("aria-pressed")).toBe("false");

  rows[0]!.click();
  await tick();
  expect(analytics.project).toBe("alpha");
  expect(rows[0]!.getAttribute("aria-pressed")).toBe("true");

  rows[0]!.click();
  await tick();
  expect(analytics.project).toBe("");
});

it("shows the row details on keyboard focus as on hover", async () => {
  analytics.projects = { projects: [project("alpha", 30)] };
  component = mount(ProjectBreakdown, { target: document.body });
  await tick();

  const row = document.querySelector<HTMLButtonElement>("button.bar-row")!;
  row.focus();
  await tick();
  expect(document.querySelector(".tooltip")?.textContent?.trim()).toBe(
    "30 messages | 2 sessions | claude: 30",
  );
  row.blur();
  await tick();
  expect(document.querySelector(".tooltip")).toBeNull();
});

it("leaves the rolled-up Other row inert", async () => {
  analytics.projects = {
    projects: Array.from({ length: 17 }, (_, i) => project(`p${i}`, 100 - i)),
  };
  component = mount(ProjectBreakdown, { target: document.body });
  await tick();

  const rows = [...document.querySelectorAll<HTMLElement>(".bar-row")];
  expect(rows).toHaveLength(16);
  expect(rows.at(-1)!.textContent).toContain("Other (2)");
  expect(rows.at(-1)!.tagName).not.toBe("BUTTON");
  expect(document.querySelectorAll("button.bar-row")).toHaveLength(15);
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
