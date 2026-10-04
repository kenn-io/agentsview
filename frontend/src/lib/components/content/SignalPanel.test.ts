// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { Session } from "../../api/types.js";

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

describe("SignalPanel", () => {
  let component: ReturnType<typeof mount> | undefined;

  afterEach(async () => {
    if (component) await unmount(component);
    component = undefined;
    document.body.innerHTML = "";
  });

  it("hosts the session's friction findings even without scoring data", async () => {
    component = mount(SignalPanel, { target: document.body, props: { session } });
    await tick();
    expect(document.body.textContent).toContain("Not enough activity");
    expect(document.querySelector(".signal-panel .friction-findings")).not.toBeNull();
  });
});
