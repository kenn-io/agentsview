// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { CallTiming } from "../../api/types/timing.js";
import CallRow from "./CallRow.svelte";

afterEach(() => {
  document.body.innerHTML = "";
});

describe("CallRow measured timing", () => {
  it.each([
    { duration: 2000, isLive: false, label: "2.0s", width: "40%" },
    { duration: 0, isLive: false, label: "0ms", width: "0%" },
    { duration: null, isLive: false, label: "unknown", width: "0%" },
    { duration: null, isLive: true, label: "running 5.0s+", width: "0%" },
    { duration: 2000, isLive: true, label: "running 5.0s+", width: "40%" },
  ])(
    "renders $label from the call when live=$isLive",
    async ({ duration, isLive, label, width }) => {
      const call: CallTiming = {
        tool_use_id: "call-1",
        tool_name: "Bash",
        category: "Bash",
        duration_ms: duration,
        is_parallel: true,
        input_preview: "pwd",
      };
      const component = mount(CallRow, {
        target: document.body,
        props: {
          call,
          barWidthPct: 40,
          isLive,
          liveDurationMs: 5000,
        },
      });
      await tick();

      expect(document.querySelector(".cd")?.textContent?.trim()).toBe(label);
      expect(document.querySelector<HTMLElement>(".cbar")?.style.width).toBe(width);
      unmount(component);
    },
  );

  it("keeps unknown child navigation and expansion separate", async () => {
    const onClick = vi.fn();
    const onChevronClick = vi.fn();
    const component = mount(CallRow, {
      target: document.body,
      props: {
        call: {
          tool_use_id: "task-1",
          tool_name: "Task",
          category: "Task",
          duration_ms: null,
          subagent_session_id: "child-1",
          is_parallel: false,
          input_preview: "review",
        },
        barWidthPct: 0,
        onClick,
        onChevronClick,
      },
    });
    await tick();

    document.querySelector<HTMLButtonElement>("button.chev")!.click();
    expect(onChevronClick).toHaveBeenCalledOnce();
    expect(onClick).not.toHaveBeenCalled();
    document.querySelector<HTMLElement>(".call")!.click();
    expect(onClick).toHaveBeenCalledOnce();
    expect(document.querySelector(".cd")?.textContent?.trim()).toBe("unknown");
    unmount(component);
  });
});
