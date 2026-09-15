// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { CallTiming } from "../../api/types/timing.js";
import CallGroup from "./CallGroup.svelte";

afterEach(() => {
  document.body.innerHTML = "";
});

describe("CallGroup measured timing", () => {
  it.each([false, true])("keeps parallel durations separate when live=%s", async (isLive) => {
    const calls: CallTiming[] = [
      {
        tool_use_id: "a",
        tool_name: "Bash",
        category: "Bash",
        duration_ms: 2000,
        is_parallel: true,
        input_preview: "pwd",
      },
      {
        tool_use_id: "b",
        tool_name: "Task",
        category: "Task",
        duration_ms: null,
        subagent_session_id: "child-1",
        is_parallel: true,
        input_preview: "review",
      },
    ];
    const onCallClick = vi.fn();
    const onSubagentExpand = vi.fn();
    const component = mount(CallGroup, {
      target: document.body,
      props: {
        calls,
        isLive,
        liveDurationMs: 6000,
        barScalePct: () => 40,
        onCallClick,
        onSubagentExpand,
        expandedSubagentIds: new Set<string>(),
      },
    });
    await tick();

    expect([...document.querySelectorAll(".cd")].map((el) => el.textContent?.trim())).toEqual([
      "2.0s",
      isLive ? "running 6.0s+" : "unknown",
    ]);
    expect(
      [...document.querySelectorAll<HTMLElement>(".cbar")].map((el) => el.style.width),
    ).toEqual(["40%", "0%"]);
    expect(document.querySelector(".cg-header")?.textContent).toContain("2 calls");
    expect(document.querySelector(".cg-header")?.textContent).not.toContain("5.0s");
    document.querySelector<HTMLElement>(".call")!.click();
    expect(onCallClick).toHaveBeenCalledWith(calls[0]);
    document.querySelector<HTMLButtonElement>("button.chev")!.click();
    expect(onSubagentExpand).toHaveBeenCalledWith(calls[1]);
    expect(onCallClick).toHaveBeenCalledOnce();
    unmount(component);
  });
});
