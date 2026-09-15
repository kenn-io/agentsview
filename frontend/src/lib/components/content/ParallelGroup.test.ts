// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { ToolCall } from "../../api/types.js";
import type { CallTiming } from "../../api/types/timing.js";
import { setLocale } from "../../i18n/index.js";
// @ts-ignore
import ParallelGroup from "./ParallelGroup.svelte";

function makeToolCall(id: string): ToolCall {
  return {
    tool_use_id: id,
    tool_name: "Read",
    category: "Read",
    input_json: "{}",
    result_content: "",
  };
}

afterEach(() => {
  setLocale("en");
  document.body.innerHTML = "";
});

describe("ParallelGroup", () => {
  it("shows a closed ordinary call and an unknown child call independently", async () => {
    const callTimingByID = new Map<string, CallTiming>([
      [
        "a",
        {
          tool_use_id: "a",
          tool_name: "Read",
          category: "Read",
          duration_ms: 2000,
          is_parallel: true,
          input_preview: "main.go",
        },
      ],
      [
        "b",
        {
          tool_use_id: "b",
          tool_name: "Task",
          category: "Task",
          duration_ms: null,
          is_parallel: true,
          input_preview: "review",
          subagent_session_id: "child-1",
        },
      ],
    ]);
    const component = mount(ParallelGroup, {
      target: document.body,
      props: {
        toolCalls: [makeToolCall("a"), makeToolCall("b")],
        callTimingByID,
      },
    });
    await tick();

    expect(
      [...document.querySelectorAll(".tool-duration")].map((el) => el.textContent?.trim()),
    ).toEqual(["2.0s", "unknown"]);
    expect(document.querySelector(".pg-header")?.textContent).not.toContain("5.0s");
    expect(document.querySelector(".pg-count")?.textContent?.trim()).toBe("2 calls");
    unmount(component);
  });

  it("renders parallel tool group labels in Simplified Chinese", async () => {
    setLocale("zh-CN");
    const component = mount(ParallelGroup, {
      target: document.body,
      props: {
        toolCalls: [makeToolCall("a"), makeToolCall("b")],
      },
    });
    await tick();

    expect(document.querySelector(".pg-label")?.textContent?.trim()).toBe("并行");
    expect(document.querySelector(".pg-count")?.textContent?.trim()).toBe("2 次调用");
    expect(
      [...document.querySelectorAll(".tool-duration")].map((el) => el.textContent?.trim()),
    ).toEqual(["未知", "未知"]);

    unmount(component);
  });
});
