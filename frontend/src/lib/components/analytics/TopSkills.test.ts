// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
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

  function headerLabels(): string[] {
    return [...document.querySelectorAll("thead th")].map((th) =>
      th.textContent!.trim(),
    );
  }

  function disclosure(): HTMLButtonElement {
    const button = document.querySelector<HTMLButtonElement>(
      "tbody button[aria-expanded]",
    );
    expect(button).toBeTruthy();
    return button!;
  }

  function detailRow(button: HTMLButtonElement): HTMLElement {
    const id = button.getAttribute("aria-controls");
    expect(id).toBeTruthy();
    const row = document.getElementById(id!);
    expect(row).toBeTruthy();
    return row!;
  }

  it("renders one table row per skill under column headers", async () => {
    const component = mountWithData();
    await tick();

    expect(document.body.textContent).toContain("Top skills");
    expect(document.body.textContent).toContain("9 calls");
    expect(document.body.textContent).toContain("2 skills");
    expect(headerLabels()).toEqual([
      "Skill",
      "Share",
      "Calls",
      "Sessions",
      "Last used",
    ]);

    const row = disclosure().closest("tr")!;
    const cells = [...row.children].map((cell) => cell.textContent!.trim());
    expect(cells[0]).toBe("skill-creator");
    expect(cells.slice(2, 4)).toEqual(["7", "3"]);
    // A dated skill shows a date, not the "Never" placeholder; its text
    // follows the browser's locale and time zone.
    expect(cells[4]).toMatch(/\d/);
    await unmount(component);
  });

  it("keeps the agent and project breakdown closed until the row is expanded", async () => {
    const component = mountWithData();
    await tick();

    const button = disclosure();
    const details = detailRow(button);
    expect(button.getAttribute("aria-expanded")).toBe("false");
    expect(details.hidden).toBe(true);

    button.click();
    await tick();

    expect(button.getAttribute("aria-expanded")).toBe("true");
    expect(details.hidden).toBe(false);
    const groups = [...details.querySelectorAll("ul")].map((list) => ({
      label: document.getElementById(list.getAttribute("aria-labelledby")!)!
        .textContent!.trim(),
      items: [...list.querySelectorAll("li")].map((item) =>
        item.textContent!.replace(/\s+/g, " ").trim(),
      ),
    }));
    expect(groups).toEqual([
      {
        label: "Agents",
        items: ["codex 5 71%", "cursor 1 14%", "claude 2 29%"],
      },
      { label: "Projects", items: ["agentsview 4", "notes 3"] },
    ]);

    button.click();
    await tick();

    expect(button.getAttribute("aria-expanded")).toBe("false");
    expect(details.hidden).toBe(true);
    await unmount(component);
  });

  it("does not expose incomplete click-to-search behavior", async () => {
    const component = mountWithData();
    await tick();

    disclosure().click();
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
    const retrySpy = vi.spyOn(analytics, "fetchSkills").mockResolvedValue("ok");
    const component = mount(TopSkills, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Failed to load");
    document.querySelector<HTMLButtonElement>(".retry-btn")!.click();
    await tick();

    expect(retrySpy).toHaveBeenCalledOnce();

    await unmount(component);
  });
});
