// @vitest-environment jsdom
import {
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import TopSkills from "./TopSkills.svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { ui } from "../../stores/ui.svelte.js";

describe("TopSkills", () => {
  afterEach(() => {
    analytics.skills = null;
    analytics.project = "";
    // @ts-ignore
    analytics.errors = {
      ...analytics.errors,
      skills: null,
    };
    ui.activeModal = null;
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  function mountWithData() {
    analytics.project = "agentsview";
    analytics.skills = {
      total_skill_calls: 9,
      distinct_skills: 2,
      by_skill: [
        {
          skill_name: "skill-creator",
          call_count: 7,
          session_count: 3,
          agent_breakdown: [
            { agent: "codex", count: 5 },
            { agent: "cursor", count: 1 },
            { agent: "claude", count: 2 },
          ],
          project_breakdown: [
            { project: "agentsview", count: 4 },
            { project: "notes", count: 3 },
          ],
          last_used_at: "2024-01-15T00:00:00Z",
          pct: 77.8,
        },
      ],
      trend: [
        {
          date: "2024-01-01",
          by_skill: { "skill-creator": 2 },
        },
        {
          date: "2024-01-08",
          by_skill: { "skill-creator": 5 },
        },
      ],
    };
    // @ts-ignore
    analytics.errors = {
      ...analytics.errors,
      skills: null,
    };

    return mount(TopSkills, { target: document.body });
  }

  it("renders skill usage and breakdowns", async () => {
    const component = mountWithData();
    await tick();

    expect(document.body.textContent).toContain("Top Skills");
    expect(document.body.textContent).toContain("9 calls");
    expect(document.body.textContent).toContain("2 skills");
    expect(document.body.textContent).toContain("skill-creator");
    expect(document.body.textContent).toContain("7");
    expect(document.body.textContent).toContain("3 sessions");
    expect(document.body.textContent).toContain("Agents");
    expect(document.body.textContent).toContain("codex");
    expect(document.body.textContent).toContain("5");
    expect(document.body.textContent).toContain("71%");
    expect(document.body.textContent).toContain("cursor");
    expect(document.body.textContent).toContain("1");
    expect(document.body.textContent).toContain("14%");
    expect(document.body.textContent).toContain("claude");
    expect(document.body.textContent).toContain("2");
    expect(document.body.textContent).toContain("29%");
    expect(document.body.textContent).toContain("Projects: agentsview: 4, notes: 3");
    await unmount(component);
  });

  it("does not expose incomplete click-to-search behavior", async () => {
    const component = mountWithData();
    await tick();

    const row = document.querySelector<HTMLElement>(".skill-row");
    expect(row).toBeTruthy();
    expect(row!.tagName).not.toBe("BUTTON");
    row!.click();
    await tick();

    expect(ui.activeModal).toBeNull();

    await unmount(component);
  });

  it("renders empty state", async () => {
    analytics.skills = {
      total_skill_calls: 0,
      distinct_skills: 0,
      by_skill: [],
      trend: [],
    };
    const component = mount(TopSkills, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("No skill usage data");

    await unmount(component);
  });

  it("renders error state and retries", async () => {
    analytics.skills = null;
    // @ts-ignore
    analytics.errors = {
      ...analytics.errors,
      skills: "Failed to load",
    };
    const retrySpy = vi
      .spyOn(analytics, "fetchSkills")
      .mockResolvedValue("ok");
    const component = mount(TopSkills, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Failed to load");
    document.querySelector<HTMLButtonElement>(".retry-btn")!.click();
    await tick();

    expect(retrySpy).toHaveBeenCalledOnce();

    await unmount(component);
  });
});
