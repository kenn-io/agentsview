// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { fireEvent, screen, within } from "@testing-library/svelte";
import { mount, tick, unmount } from "svelte";
import SessionHealthSection from "./SessionHealthSection.svelte";

const signals = {
  scored_sessions: 12,
  unscored_sessions: 0,
  avg_health_score: 80,
  grade_distribution: {},
  outcome_distribution: {},
  trend: [],
  tool_health: { failure_rate: 0, sessions_with_failures: 0 },
  context_health: { sessions_with_compaction: 0, avg_compaction_count: 0 },
  by_agent: [
    { agent: "zeta", session_count: 2, avg_health_score: 80.4, completed_rate: 90 },
    { agent: "alpha", session_count: 10, avg_health_score: 80.1, completed_rate: 20 },
  ],
  by_project: [
    { project: "project-zeta", session_count: 20, avg_health_score: 80.1, completed_rate: 90 },
    { project: "project-alpha", session_count: 10, avg_health_score: 80.4, completed_rate: 20 },
  ],
};
const analytics = vi.hoisted(() => ({
  signals: null as unknown,
  errors: { signals: null as string | null },
  querying: { signals: false },
  fetchSignals: vi.fn(),
}));
vi.mock("../../stores/analytics.svelte.js", () => ({ analytics }));
afterEach(() => {
  document.body.innerHTML = "";
  analytics.signals = signals;
  analytics.errors.signals = null;
  vi.clearAllMocks();
});
analytics.signals = signals;
it("sorts agent scores and project names independently", async () => {
  const component = mount(SessionHealthSection, { target: document.body });
  await tick();
  const [agents, projects] = screen.getAllByRole("table");
  const names = (table: HTMLElement) =>
    Array.from(table.querySelectorAll("tbody tr td:first-child"), (cell) =>
      cell.textContent?.trim(),
    );
  expect(names(agents!)).toEqual(["alpha", "zeta"]);
  expect(names(projects!)).toEqual(["project-zeta", "project-alpha"]);
  await fireEvent.click(within(agents!).getByRole("button", { name: "Avg Score" }));
  expect(names(agents!)).toEqual(["zeta", "alpha"]);
  expect(names(projects!)).toEqual(["project-zeta", "project-alpha"]);
  await fireEvent.click(within(projects!).getByRole("button", { name: "Project" }));
  expect(names(projects!)).toEqual(["project-alpha", "project-zeta"]);
  expect(names(agents!)).toEqual(["zeta", "alpha"]);
  unmount(component);
});

it("keeps scored results beside a refresh failure and retries a first-load failure", async () => {
  analytics.errors.signals = "refresh failed";
  const component = mount(SessionHealthSection, { target: document.body });
  await tick();
  expect(screen.getByText("refresh failed")).toBeTruthy();
  expect(screen.getAllByRole("table")).toHaveLength(2);
  await unmount(component);

  analytics.signals = null;
  const failed = mount(SessionHealthSection, { target: document.body });
  try {
    await tick();
    expect(screen.getByText("refresh failed")).toBeTruthy();
    expect(screen.queryAllByRole("table")).toHaveLength(0);
    await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(analytics.fetchSignals).toHaveBeenCalledOnce();
  } finally {
    await unmount(failed);
  }
});

it("stays hidden when a successful response has no sessions", async () => {
  analytics.signals = { ...signals, scored_sessions: 0, unscored_sessions: 0 };
  const component = mount(SessionHealthSection, { target: document.body });
  await tick();
  expect(document.body.textContent?.trim()).toBe("");
  unmount(component);
});
