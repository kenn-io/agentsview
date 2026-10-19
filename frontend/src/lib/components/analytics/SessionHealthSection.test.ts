// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { fireEvent, screen, within } from "@testing-library/svelte";
import { mount, tick, unmount } from "svelte";
import SessionHealthSection from "./SessionHealthSection.svelte";

vi.mock("../../stores/analytics.svelte.js", () => ({
  analytics: {
    signals: {
      scored_sessions: 12,
      unscored_sessions: 0,
      avg_health_score: 80,
      grade_distribution: {},
      outcome_distribution: {},
      trend: [],
      tool_health: { failure_rate: 0, sessions_with_failures: 0 },
      context_health: { sessions_with_compaction: 0, avg_compaction_count: 0 },
      by_agent: [
        { agent: "zeta", session_count: 2, avg_health_score: 80.1, completed_rate: 90 },
        { agent: "alpha", session_count: 10, avg_health_score: 80.4, completed_rate: 20 },
        { agent: "empty", session_count: 1, avg_health_score: null, completed_rate: 0 },
      ],
      by_project: [
        { project: "project-zeta", session_count: 2, avg_health_score: 80.1, completed_rate: 90 },
        { project: "project-alpha", session_count: 10, avg_health_score: 80.4, completed_rate: 20 },
      ],
    },
  },
}));
afterEach(() => {
  document.body.innerHTML = "";
});
it("sorts raw health values independently and restores the session-count baseline", async () => {
  const component = mount(SessionHealthSection, { target: document.body });
  await tick();
  const [agents, projects] = screen.getAllByRole("table");
  const names = (table: HTMLElement) =>
    Array.from(table.querySelectorAll("tbody tr td:first-child"), (cell) =>
      cell.textContent?.trim(),
    );
  expect(names(agents!)).toEqual(["alpha", "zeta", "empty"]);
  const score = within(agents!).getByRole("button", { name: "Avg Score" });
  await fireEvent.click(score);
  expect(names(agents!)).toEqual(["alpha", "zeta", "empty"]);
  await fireEvent.click(score);
  expect(names(agents!)).toEqual(["zeta", "alpha", "empty"]);
  await fireEvent.click(score);
  expect(names(agents!)).toEqual(["alpha", "zeta", "empty"]);
  await fireEvent.click(within(agents!).getByRole("button", { name: "Completed" }));
  expect(names(agents!)).toEqual(["zeta", "alpha", "empty"]);
  expect(names(projects!)).toEqual(["project-alpha", "project-zeta"]);
  await fireEvent.click(within(projects!).getByRole("button", { name: "Project" }));
  expect(names(projects!)).toEqual(["project-alpha", "project-zeta"]);
  await fireEvent.click(within(projects!).getByRole("button", { name: "Project" }));
  expect(names(projects!)).toEqual(["project-zeta", "project-alpha"]);
  expect(names(agents!)).toEqual(["zeta", "alpha", "empty"]);
  unmount(component);
});
