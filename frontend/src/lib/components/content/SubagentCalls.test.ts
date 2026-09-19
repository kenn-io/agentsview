// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { DbSessionTiming as SessionTiming } from "../../api/generated/index.js";
import SubagentCalls from "./SubagentCalls.svelte";

afterEach(() => {
  document.body.innerHTML = "";
});

describe("SubagentCalls measured timing", () => {
  it.each([false, true])(
    "preserves measured and unknown nested calls when running=%s",
    async (running) => {
      const timing: SessionTiming = {
        session_id: "child-1",
        total_duration_ms: 6000,
        tool_duration_ms: 2000,
        turn_count: 2,
        tool_call_count: 3,
        subagent_count: 0,
        slowest_call: null,
        by_category: [],
        activity: [],
        activity_totals: { tool_ms: 2000, unattributed_ms: 4000 },
        running,
        turns: [
          {
            message_id: 1,
            ordinal: 1,
            started_at: "2026-08-01T00:00:00Z",
            duration_ms: 5000,
            primary_category: "Mixed",
            calls: [
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
                tool_name: "Read",
                category: "Read",
                duration_ms: null,
                is_parallel: true,
                input_preview: "main.go",
              },
            ],
          },
          {
            message_id: 2,
            ordinal: 2,
            started_at: "2026-08-01T00:00:05Z",
            duration_ms: running ? null : 1000,
            primary_category: "Task",
            calls: [
              {
                tool_use_id: "c",
                tool_name: "Task",
                category: "Task",
                duration_ms: null,
                is_parallel: false,
                input_preview: "review",
                subagent_session_id: "nested-child",
              },
            ],
          },
        ],
      };
      const component = mount(SubagentCalls, {
        target: document.body,
        props: { timing, barScalePct: () => 50, categoryFilter: "Read" },
      });
      await tick();

      const durations = [...document.querySelectorAll(".cd")].map((el) => el.textContent?.trim());
      expect(durations.slice(0, 2)).toEqual(["2.0s", "unknown"]);
      if (running) expect(durations[2]).toMatch(/^running /);
      else expect(durations[2]).toBe("unknown");
      expect(
        [...document.querySelectorAll<HTMLElement>(".cbar")].map((el) => el.style.width),
      ).toEqual(["50%", "0%", "0%"]);
      expect(document.querySelector(".sa-eh-meta")?.textContent).toContain("3 calls");
      expect(document.querySelector(".cgroup")?.classList.contains("dimmed")).toBe(false);
      expect(document.querySelector("button.chev")).toBeNull();
      unmount(component);
    },
  );
});
