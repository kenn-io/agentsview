// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { Session } from "../../api/types.js";
import { FrictionService, type VersionInfo } from "../../api/generated/index.js";
import { sync } from "../../stores/sync.svelte.js";

vi.mock("../../api/generated/index.js", () => ({
  FrictionService: {
    getApiV1FrictionFindings: vi.fn().mockResolvedValue({ findings: [], next_cursor: "" }),
  },
}));

// @ts-ignore
import SignalPanel from "./SignalPanel.svelte";

const session = {
  id: "test-session-medium-8",
  health_score: null,
  health_grade: null,
  health_score_basis: [],
  health_penalties: null,
  compaction_count: 0,
  mid_task_compaction_count: 0,
  outcome: "unknown",
  outcome_confidence: "",
} as unknown as Session;

function version(frictionAvailable: boolean): VersionInfo {
  return {
    api_version: 1,
    data_version: 1,
    friction_available: frictionAvailable,
    friction_build_available: frictionAvailable,
    session_stats_available: false,
    insight_generation_available: false,
    kata_available: false,
    kata_filing_available: false,
    version: "dev",
    commit: "unknown",
    build_date: "",
    read_only: false,
  };
}

describe("SignalPanel", () => {
  let component: ReturnType<typeof mount> | undefined;

  afterEach(async () => {
    if (component) await unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    sync.serverVersion = null;
  });

  it("hosts the session's friction findings even without scoring data", async () => {
    sync.serverVersion = version(true);
    vi.mocked(FrictionService.getApiV1FrictionFindings).mockClear();
    component = mount(SignalPanel, { target: document.body, props: { session } });
    await tick();
    expect(document.body.textContent).toContain("Not enough activity");
    expect(document.querySelector(".signal-panel .friction-findings")).not.toBeNull();
    expect(FrictionService.getApiV1FrictionFindings).toHaveBeenCalledOnce();
  });

  it("does not request session findings when Friction is unavailable", async () => {
    sync.serverVersion = version(false);
    vi.mocked(FrictionService.getApiV1FrictionFindings).mockClear();
    component = mount(SignalPanel, { target: document.body, props: { session } });
    await tick();

    expect(document.querySelector(".signal-panel .friction-findings")).toBeNull();
    expect(FrictionService.getApiV1FrictionFindings).not.toHaveBeenCalled();
  });

  it("waits for server capabilities before requesting session findings", async () => {
    sync.serverVersion = null;
    vi.mocked(FrictionService.getApiV1FrictionFindings).mockClear();
    component = mount(SignalPanel, { target: document.body, props: { session } });
    await tick();

    expect(document.querySelector(".signal-panel .friction-findings")).toBeNull();
    expect(FrictionService.getApiV1FrictionFindings).not.toHaveBeenCalled();
  });
});
