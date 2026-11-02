import { reportErrorShown } from "../../utils/telemetry.js";
vi.mock("../../utils/telemetry.js", () => ({ reportErrorShown: vi.fn() }));
// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { insights } from "../../stores/insights.svelte.js";
import { router } from "../../stores/router.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { MetadataService } from "../../api/generated/index.js";
import { sync } from "../../stores/sync.svelte.js";

// @ts-ignore
import QualityPage from "./QualityPage.svelte";

describe("QualityPage", () => {
  let component: ReturnType<typeof mount> | undefined;
  let insightLoadCalls = 0;

  beforeEach(() => {
    vi.mocked(reportErrorShown).mockClear();
    analytics.signals = null;
    analytics.errors.signals = null;
    router.route = "quality";
    router.params = {};
    vi.spyOn(analytics, "fetchSignalsForQuality").mockResolvedValue();
    insightLoadCalls = 0;
    vi.spyOn(insights, "load").mockImplementation(async () => {
      insightLoadCalls += 1;
    });
  });

  afterEach(async () => {
    if (component) await unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    router.route = "sessions";
    router.params = {};
    sync.serverVersion = null;
    vi.restoreAllMocks();
  });

  it("loads quality once with the already-selected machine from a label deep link", async () => {
    const requests: string[] = [];
    vi.mocked(analytics.fetchSignalsForQuality).mockImplementation(async () => {
      requests.push(analytics.machine);
    });
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    const metadata =
      Promise.withResolvers<Awaited<ReturnType<typeof MetadataService.getApiV1Machines>>>();
    vi.spyOn(MetadataService, "getApiV1Machines").mockReturnValue(metadata.promise);
    router.params = { machine: "Workstation", date_from: "2026-03-01", date_to: "2026-03-31" };
    const savedFilters = sessions.filters;
    const savedMachine = analytics.machine;
    analytics.machine = "id-a";
    sessions.initFromParams(router.params);

    try {
      component = mount(QualityPage, { target: document.body });
      await tick();
      expect(requests).toEqual([]);

      metadata.resolve({
        machines: ["id-a"],
        resolved_machines: ["id-a"],
        machine_labels: { "id-a": "Workstation" },
        machine_aliases: {},
      });
      await sessions.loadMachines();
      await tick();

      expect(requests).toEqual(["id-a"]);
    } finally {
      if (component) await unmount(component);
      component = undefined;
      sessions.filters = savedFilters;
      analytics.machine = savedMachine;
    }
  });

  it("links to the Friction Log from recommendations when available", async () => {
    sync.serverVersion = {
      api_version: 1,
      data_version: 1,
      insight_generation_available: false,
      friction_available: true,
      friction_build_allowed: true,
      friction_readable: true,
      session_stats_available: false,
      kata_available: false,
      version: "dev",
      commit: "unknown",
      build_date: "",
      read_only: false,
    };
    component = mount(QualityPage, { target: document.body });
    await tick();

    const link = document.querySelector<HTMLAnchorElement>("a.friction-link");
    expect(link).not.toBeNull();
    expect(link!.getAttribute("href")).toBe("/friction");
    expect(document.body.textContent).toContain("Daily digests of corrections");

    link!.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, button: 0 }));
    expect(router.route).toBe("friction");
  });

  it("omits the Friction Log card when the server does not build digests", async () => {
    sync.serverVersion = null;
    component = mount(QualityPage, { target: document.body });
    await tick();
    expect(document.querySelector("a.friction-link")).toBeNull();
  });

  it("renders deterministic quality analysis without loading generated reports", async () => {
    component = mount(QualityPage, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Deterministic Recommendations");
    expect(document.body.textContent).toContain("Quality Patterns");
    expect(insightLoadCalls).toBe(0);
  });

  it("reports the analytics kind for a visible signals failure", async () => {
    analytics.errors.signals = "Signals failed";
    component = mount(QualityPage, { target: document.body });
    await tick();
    expect(reportErrorShown).toHaveBeenCalledExactlyOnceWith("analytics");
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
