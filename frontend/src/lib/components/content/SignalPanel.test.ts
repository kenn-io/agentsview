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

  it("lists penalties largest first, whatever order the session reports them in", async () => {
    const orders = [];
    for (const health_penalties of [
      { edit_churn: 4, zz_custom: 4, tool_failures: 6 },
      { tool_failures: 6, zz_custom: 4, edit_churn: 4 },
    ]) {
      component = mount(SignalPanel, {
        target: document.body,
        props: { session: { ...session, health_score: 90, health_penalties } },
      });
      await tick();
      orders.push(
        [...document.querySelectorAll(".penalty .penalty-value")].map((el) =>
          el.parentElement!.textContent!.replace(/\s+/g, " ").trim(),
        ),
      );
      await unmount(component);
      component = undefined;
    }
    expect(orders[0]).toEqual(orders[1]);
    expect(orders[0]!.map((text) => text.split(" ")[0])).toEqual(["-6", "-4", "-4"]);
    expect(orders[0]![2]).toContain("zz_custom");
  });
});
