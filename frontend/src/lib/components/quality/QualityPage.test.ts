// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { insights } from "../../stores/insights.svelte.js";
import { router } from "../../stores/router.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { branchFilterToken } from "../../branchFilters.js";

// @ts-ignore
import QualityPage from "./QualityPage.svelte";

describe("QualityPage", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    router.route = "quality";
    router.params = {};
    vi.spyOn(analytics, "fetchSignalsForQuality").mockResolvedValue();
    vi.spyOn(insights, "load").mockResolvedValue();
  });

  afterEach(() => {
    if (component) unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    router.route = "sessions";
    router.params = {};
    sessions.filters.branch = "";
    analytics.branch = "";
    vi.restoreAllMocks();
  });

  it("renders deterministic quality analysis without loading generated reports", async () => {
    component = mount(QualityPage, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Deterministic Recommendations");
    expect(document.body.textContent).toContain("Quality Patterns");
    expect(insights.load).not.toHaveBeenCalled();
  });

  it("loads signals for the branch filter set before the page opened", async () => {
    const main = branchFilterToken("pl1:sha256:alpha", "main");
    sessions.filters.branch = main;
    const branchAtFetch: string[] = [];
    vi.mocked(analytics.fetchSignalsForQuality).mockImplementation(async () => {
      branchAtFetch.push(analytics.branch);
    });

    component = mount(QualityPage, { target: document.body });
    await tick();

    expect(analytics.branch).toBe(main);
    expect(branchAtFetch.at(-1)).toBe(main);
  });

  it("reloads signals when only the branch filter changes", async () => {
    component = mount(QualityPage, { target: document.body });
    await tick();
    vi.mocked(analytics.fetchSignalsForQuality).mockClear();

    const dev = branchFilterToken("pl1:sha256:alpha", "dev");
    sessions.filters.branch = dev;
    await tick();

    expect(analytics.branch).toBe(dev);
    expect(analytics.fetchSignalsForQuality).toHaveBeenCalledTimes(1);
  });

  it("shows refresh activity while a filtered quality query is running", async () => {
    component = mount(QualityPage, { target: document.body });
    await tick();

    analytics.querying.signals = true;
    await tick();

    const content = document.querySelector(".content");
    const refreshButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Refresh quality"]',
    );

    expect(content?.getAttribute("aria-busy")).toBe("true");
    expect(content?.querySelector(".query-progress")).not.toBeNull();
    expect(refreshButton?.disabled).toBe(true);
  });

  it("shows Quality freshness instead of dashboard freshness", async () => {
    const now = new Date("2026-06-15T15:00:00Z").getTime();
    vi.spyOn(Date, "now").mockReturnValue(now);
    analytics.lastUpdatedAt = now - 3 * 60_000;
    analytics.qualityLastUpdatedAt = now - 7 * 60_000;

    component = mount(QualityPage, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Updated 7m ago");
    expect(document.body.textContent).not.toContain("Updated 3m ago");
  });
});
