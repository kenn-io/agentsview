// @vitest-environment jsdom
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import SkillTrend from "./SkillTrend.svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { setLocale } from "../../i18n/index.js";

describe("SkillTrend", () => {
  beforeEach(() => {
    setLocale("en");
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
  });

  afterEach(() => {
    setLocale("en");
    analytics.skills = null;
    analytics.skillsGranularity = "week";
    // @ts-ignore
    analytics.errors = {
      ...analytics.errors,
      skills: null,
    };
    document.body.innerHTML = "";
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  function skillsResponse(
    trend: { date: string; by_skill: Record<string, number> }[],
  ) {
    return {
      total_skill_calls: 0,
      distinct_skills: 0,
      by_skill: [],
      trend,
    };
  }

  function mountWithData() {
    analytics.skills = skillsResponse([
      {
        date: "2024-01-01",
        by_skill: { commit: 4, review: 2 },
      },
      {
        date: "2024-01-08",
        by_skill: { commit: 6, deploy: 1 },
      },
    ]);
    // @ts-ignore
    analytics.errors = {
      ...analytics.errors,
      skills: null,
    };

    return mount(SkillTrend, { target: document.body });
  }

  it("renders one line per series with a legend", async () => {
    const component = mountWithData();
    await tick();

    expect(document.body.textContent).toContain("Skill Usage Over Time");

    const chips = document.querySelectorAll<HTMLButtonElement>(
      ".legend-chip",
    );
    expect(chips).toHaveLength(3);
    // Legend ordered by total volume: commit (10), review (2), deploy (1).
    expect(chips[0]!.textContent).toContain("commit");
    expect(chips[0]!.textContent).toContain("10");
    expect(chips[1]!.textContent).toContain("review");
    expect(chips[2]!.textContent).toContain("deploy");

    const lines = document.querySelectorAll(".series-line");
    expect(lines).toHaveLength(3);
    expect(document.body.textContent).toContain("Jan 1");
    expect(document.body.textContent).toContain("Jan 8");

    await unmount(component);
  });

  it("hides a series line when its legend chip is toggled", async () => {
    const component = mountWithData();
    await tick();

    const chips = document.querySelectorAll<HTMLButtonElement>(
      ".legend-chip",
    );
    expect(chips[0]!.getAttribute("aria-pressed")).toBe("true");
    chips[0]!.click();
    await tick();

    expect(chips[0]!.getAttribute("aria-pressed")).toBe("false");
    expect(document.querySelectorAll(".series-line")).toHaveLength(2);

    chips[0]!.click();
    await tick();
    expect(document.querySelectorAll(".series-line")).toHaveLength(3);

    await unmount(component);
  });

  it("keeps survivor colors stable when a series is hidden", async () => {
    const component = mountWithData();
    await tick();

    const lineStyles = () =>
      [...document.querySelectorAll<SVGPathElement>(".series-line")]
        .map((line) => line.getAttribute("style") ?? "");
    // "review" is series slot 2 while all three lines are visible.
    expect(lineStyles()[1]).toContain("--chart-series-2");

    document
      .querySelectorAll<HTMLButtonElement>(".legend-chip")[0]!
      .click();
    await tick();

    // With "commit" hidden, "review" keeps its slot-2 hue.
    expect(lineStyles()[0]).toContain("--chart-series-2");

    await unmount(component);
  });

  it("folds skills past the series cap into Other", async () => {
    const bySkill: Record<string, number> = {};
    for (let i = 0; i < 8; i++) {
      bySkill[`skill-${i}`] = 8 - i;
    }
    analytics.skills = skillsResponse([
      { date: "2024-01-01", by_skill: bySkill },
      { date: "2024-01-08", by_skill: bySkill },
    ]);
    const component = mount(SkillTrend, { target: document.body });
    await tick();

    const chips = document.querySelectorAll<HTMLButtonElement>(
      ".legend-chip",
    );
    expect(chips).toHaveLength(7);
    expect(chips[6]!.textContent).toContain("Other");
    // skill-6 (2) + skill-7 (1) fold into Other in both buckets.
    expect(chips[6]!.textContent).toContain("6");

    expect(document.querySelectorAll(".series-line")).toHaveLength(7);

    await unmount(component);
  });

  it("shows a crosshair tooltip listing every visible series", async () => {
    const component = mountWithData();
    await tick();

    const svg = document.querySelector<SVGElement>(".chart-svg")!;
    svg.dispatchEvent(
      new MouseEvent("mousemove", {
        bubbles: true,
        clientX: 0,
        clientY: 20,
      }),
    );
    await tick();

    const tooltip = document.querySelector(".tooltip")!;
    expect(tooltip).toBeTruthy();
    expect(tooltip.textContent).toContain("Jan 1, 2024");
    const rows = tooltip.querySelectorAll(".tooltip-row");
    expect(rows).toHaveLength(3);
    // Rows sorted by value: commit 4, review 2, deploy 0.
    expect(rows[0]!.textContent).toContain("commit");
    expect(rows[0]!.textContent).toContain("4");
    expect(rows[1]!.textContent).toContain("review");
    expect(rows[2]!.textContent).toContain("deploy");
    expect(document.querySelectorAll(".crosshair")).toHaveLength(1);

    document.querySelector<HTMLElement>(".chart")!
      .dispatchEvent(new MouseEvent("mouseleave"));
    await tick();
    expect(document.querySelector(".tooltip")).toBeNull();

    await unmount(component);
  });

  it("exposes trend buckets to keyboard and assistive technology", async () => {
    const component = mountWithData();
    await tick();

    const chart = document.querySelector<HTMLElement>(".chart")!;
    expect(chart.getAttribute("role")).toBe("slider");
    expect(chart.getAttribute("tabindex")).toBe("0");
    expect(chart.getAttribute("aria-describedby")).toBe(
      "skill-trend-data",
    );

    const dataTable = document.querySelector("#skill-trend-data")!;
    expect(dataTable.textContent).toContain("commit");
    expect(dataTable.textContent).toContain("Jan 1, 2024");
    expect(dataTable.textContent).toContain("4");

    chart.dispatchEvent(new FocusEvent("focus"));
    await tick();
    expect(document.querySelector(".tooltip-date")?.textContent)
      .toContain("Jan 1, 2024");

    chart.dispatchEvent(
      new KeyboardEvent("keydown", {
        key: "ArrowRight",
        bubbles: true,
      }),
    );
    await tick();
    expect(document.querySelector(".tooltip-date")?.textContent)
      .toContain("Jan 8, 2024");

    await unmount(component);
  });

  it("formats visible dates with the active locale", async () => {
    setLocale("zh-CN");
    const component = mountWithData();
    await tick();

    const chart = document.querySelector<HTMLElement>(".chart")!;
    chart.dispatchEvent(
      new MouseEvent("mousemove", {
        bubbles: true,
        clientX: 0,
        clientY: 20,
      }),
    );
    await tick();

    expect(document.querySelector(".tooltip-date")?.textContent)
      .toContain("2024年1月1日");

    await unmount(component);
  });

  it("requests granularity changes through the shared picker", async () => {
    const fetchSpy = vi
      .spyOn(analytics, "fetchSkills")
      .mockResolvedValue("ok");
    const component = mountWithData();
    await tick();

    const monthBtn = [
      ...document.querySelectorAll<HTMLButtonElement>(
        ".trend-header button",
      ),
    ].find((b) => b.textContent?.trim() === "Month");
    expect(monthBtn).toBeTruthy();
    monthBtn!.click();
    await tick();

    expect(fetchSpy).toHaveBeenCalledOnce();
    expect(fetchSpy).toHaveBeenCalledWith("month");

    await unmount(component);
  });

  it("renders empty state", async () => {
    analytics.skills = skillsResponse([]);
    const component = mount(SkillTrend, { target: document.body });
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
    const component = mount(SkillTrend, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Failed to load");
    document.querySelector<HTMLButtonElement>(".retry-btn")!.click();
    await tick();

    expect(retrySpy).toHaveBeenCalledOnce();

    await unmount(component);
  });
});
