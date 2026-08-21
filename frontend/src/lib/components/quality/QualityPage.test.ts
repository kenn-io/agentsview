// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { insights } from "../../stores/insights.svelte.js";
import { router } from "../../stores/router.svelte.js";

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
    vi.restoreAllMocks();
  });

  it("renders deterministic quality analysis without loading generated reports", async () => {
    component = mount(QualityPage, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain(
      "Deterministic Recommendations",
    );
    expect(document.body.textContent).toContain("Quality Patterns");
    expect(insights.load).not.toHaveBeenCalled();
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
});
