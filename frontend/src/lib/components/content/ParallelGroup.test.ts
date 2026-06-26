// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { ToolCall } from "../../api/types.js";
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
  it("renders parallel tool group labels in Simplified Chinese", async () => {
    setLocale("zh-CN");
    const component = mount(ParallelGroup, {
      target: document.body,
      props: {
        toolCalls: [makeToolCall("a"), makeToolCall("b")],
        turnDurationMs: 2500,
      },
    });
    await tick();

    expect(document.querySelector(".pg-label")?.textContent?.trim()).toBe(
      "并行",
    );
    expect(document.querySelector(".pg-count")?.textContent?.trim()).toBe(
      "2 次调用",
    );
    expect(document.querySelector(".pg-upper")?.textContent?.trim()).toBe(
      "每个 ≤ 2.5s",
    );

    unmount(component);
  });
});
