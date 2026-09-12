// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount, type ComponentProps } from "svelte";
import type { ToolCall } from "../../api/types.js";
import { setLocale } from "../../i18n/index.js";
import retainedFixtureSource from "../../utils/__fixtures__/retained-tool-image-1735.json?raw";
const SMALL_PNG_DATA_URI =
  "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=";
import ToolBlock from "./ToolBlock.svelte";

const copyToClipboardMock = vi.hoisted(() => vi.fn().mockResolvedValue(true));
const searchState = vi.hoisted(() => ({ active: false, current: "", query: "" }));
vi.mock("../../utils/clipboard.js", () => ({ copyToClipboard: copyToClipboardMock }));
vi.mock("../../stores/inSessionSearch.svelte.js", () => ({
  inSessionSearch: {
    get isActive() {
      return searchState.active;
    },
    get debouncedQuery() {
      return searchState.query;
    },
    navigationRevision: 1,
    isCurrentBlock: (key: string | undefined) => !!key && key === searchState.current,
    countForBlock: (key: string | undefined) => (key && key === searchState.current ? 1 : 0),
    currentOccurrence: () => 0,
  },
}));
const components: ReturnType<typeof mount>[] = [];
async function render(props: Partial<ComponentProps<typeof ToolBlock>> = {}) {
  components.push(mount(ToolBlock, { target: document.body, props: { content: "", ...props } }));
  await tick();
}
async function click(selector: string) {
  const button = document.querySelector<HTMLButtonElement>(selector);
  expect(button).not.toBeNull();
  button!.click();
  await Promise.resolve();
  await tick();
}
async function formatted() {
  const button = Array.from(
    document.querySelectorAll<HTMLButtonElement>(".output-mode button"),
  ).find((node) => node.textContent?.trim() === "Formatted");
  expect(button).toBeDefined();
  button!.click();
  await tick();
}
function call(
  tool_name: string,
  params: Record<string, unknown> = {},
  rest: Partial<ToolCall> = {},
): ToolCall {
  return { tool_name, input_json: JSON.stringify(params), ...rest };
}
const text = (selector: string) => document.querySelector(selector)?.textContent ?? "";
const longCommand = Array.from({ length: 30 }, (_, i) => `echo hidden-line-${i}`).join("\n");
beforeEach(() => {
  setLocale("en");
  searchState.active = false;
  searchState.current = "";
  searchState.query = "";
  copyToClipboardMock.mockClear();
});
afterEach(async () => {
  for (const component of components.splice(0)) await unmount(component);
  document.body.replaceChildren();
  setLocale("en");
});

describe("ToolBlock output and controls", () => {
  it("starts with both input and output collapsed", async () => {
    await render({ content: "input", toolCall: call("Read", {}, { result_content: "output" }) });
    expect(document.querySelector(".tool-content")).toBeNull();
    await click(".tool-header");
    expect(text(".tool-content")).toBe("input");
    expect(document.querySelector(".output-content")).toBeNull();
    expect(text(".output-header")).toContain("output");
  });
  it("toggles the input and output independently", async () => {
    await render({ content: "input", toolCall: call("Read", {}, { result_content: "output" }) });
    await click(".tool-header");
    await click(".output-header");
    expect(text(".output-content")).toBe("output");
    await click(".output-header");
    expect(document.querySelector(".output-content")).toBeNull();
    expect(text(".tool-content")).toBe("input");
    await click(".tool-header");
    expect(document.querySelector(".tool-content")).toBeNull();
  });
  it.each([undefined, ""])(
    "omits output for an absent or empty result: %s",
    async (result_content) => {
      await render({ content: "input", toolCall: call("Read", {}, { result_content }) });
      await click(".tool-header");
      expect(document.querySelector(".output-header")).toBeNull();
    },
  );
  it("omits output without a structured tool call", async () => {
    await render({ content: "legacy input" });
    await click(".tool-header");
    expect(document.querySelector(".output-header")).toBeNull();
  });
  it("shows only the first result line in the collapsed preview", async () => {
    await render({ toolCall: call("Read", {}, { result_content: "first\nsecond" }) });
    await click(".tool-header");
    expect(text(".output-header .tool-preview")).toBe("first");
    expect(text(".output-header")).not.toContain("second");
  });
  it("switches raw and sanitized formatted output without changing source", async () => {
    const result = "# Heading\n\n**bold**\n<script>alert(1)</script>";
    await render({ toolCall: call("Read", {}, { result_content: result }) });
    await click(".tool-header");
    await click(".output-header");
    expect(text(".output-content")).toBe(result);
    await formatted();
    expect(text(".formatted-output h1")).toBe("Heading");
    expect(text(".formatted-output strong")).toBe("bold");
    expect(document.querySelector(".formatted-output script")).toBeNull();
    await click(".output-mode button:first-child");
    expect(text(".output-content")).toBe(result);
  });
  it("colors fenced code in formatted output without inserting search marks", async () => {
    await render({
      toolCall: call("Read", {}, { result_content: "```ts\nconst target = true;\n```" }),
    });
    await click(".tool-header");
    await click(".output-header");
    await formatted();
    await vi.waitFor(
      () => expect(document.querySelector(".formatted-output code span[style]")).not.toBeNull(),
      { timeout: 10000 },
    );
    expect(text(".formatted-output code")).toContain("const target = true;");
    expect(document.querySelector("mark")).toBeNull();
  });
  it("copies raw output while it remains collapsed", async () => {
    const result = "# raw\n**output**";
    await render({ toolCall: call("Read", {}, { result_content: result }) });
    await click(".tool-header");
    await click('button[aria-label="Copy output"]');
    expect(copyToClipboardMock).toHaveBeenCalledWith(result);
    expect(document.querySelector(".output-content")).toBeNull();
  });
  it("shows retained image placeholders in block order in output and history", async () => {
    const result =
      '[{"type":"input_text","text":"Before"},{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1},{"type":"text","text":"After"}]';
    const toolCall: ToolCall = {
      tool_name: "view_image",
      category: "Other",
      result_content: result,
      result_events: [
        {
          event_index: 0,
          status: "completed",
          source: "tool_result",
          content: result,
          content_length: result.length,
        },
      ],
    };
    await render({ toolCall });
    await tick();
    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();
    expect(document.querySelector(".output-header .tool-preview")?.textContent).toBe("Before");
    document.querySelector<HTMLButtonElement>(".output-header")!.click();
    document.querySelector<HTMLButtonElement>(".history-header")!.click();
    await tick();
    expect(document.querySelector(".output-content")?.textContent).toBe(
      "Before\n\n[Image: image/png, 3 bytes]\n\nAfter",
    );
    expect(document.querySelector(".history-content")?.textContent).toBe(
      "Before\n\n[Image: image/png, 3 bytes]\n\nAfter",
    );
    document.querySelector<HTMLButtonElement>(".output-mode button:nth-child(2)")!.click();
    await tick();
    expect(
      Array.from(document.querySelectorAll(".formatted-output p"), (p) => p.textContent),
    ).toEqual(["Before", "[Image: image/png, 3 bytes]", "After"]);
    document.querySelector<HTMLButtonElement>('button[aria-label="Copy output"]')!.click();
    await tick();
    expect(copyToClipboardMock).toHaveBeenCalledWith(result);
  });

  it("renders the constructed retained PNG between text blocks", async () => {
    const result = retainedFixtureSource;
    const blocks = JSON.parse(result) as Array<{ image_url?: string }>;
    const imageURL = blocks[1]?.image_url;
    expect(imageURL).toMatch(/^data:image\/png;base64,/);
    expect(new TextEncoder().encode(result).byteLength).toBe(1_441_138);

    const toolCall: ToolCall = {
      tool_name: "view_image",
      category: "Other",
      result_content: result,
    };
    await render({ toolCall });
    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();
    document.querySelector<HTMLButtonElement>(".output-header")!.click();
    await tick();

    const raw = document.querySelector(".output-content");
    expect(raw?.querySelector("img")).toBeNull();
    expect(raw?.textContent).toBe(result);

    document.querySelector<HTMLButtonElement>(".output-mode button:nth-child(2)")!.click();
    await tick();

    const formatted = document.querySelector(".formatted-output");
    expect(formatted).not.toBeNull();
    expect(formatted!.querySelectorAll("img")).toHaveLength(1);
    expect(formatted!.querySelector("img")?.getAttribute("src")).toBe(imageURL);
    expect(Array.from(formatted!.querySelectorAll("p"), (p) => p.textContent)).toEqual([
      "Before",
      "",
      "After",
    ]);
    expect(formatted!.textContent).not.toContain("input_image");
    expect(formatted!.textContent).not.toContain(imageURL!);

    document.querySelector<HTMLButtonElement>(".output-mode button:nth-child(1)")!.click();
    await tick();
    expect(document.querySelector(".output-content")?.textContent).toBe(result);

    document.querySelector<HTMLButtonElement>('button[aria-label="Copy output"]')!.click();
    await tick();
    expect(copyToClipboardMock).toHaveBeenCalledWith(result);
  });

  it("renders an image-only retained result", async () => {
    const result = JSON.stringify([{ type: "input_image", image_url: SMALL_PNG_DATA_URI }]);
    const toolCall: ToolCall = {
      tool_name: "view_image",
      category: "Other",
      result_content: result,
    };
    await render({ toolCall });
    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();
    document.querySelector<HTMLButtonElement>(".output-header")!.click();
    await tick();
    document.querySelector<HTMLButtonElement>(".output-mode button:nth-child(2)")!.click();
    await tick();

    const formatted = document.querySelector(".formatted-output");
    expect(formatted?.querySelectorAll("img")).toHaveLength(1);
    expect(formatted?.textContent).not.toContain("input_image");
  });

  it("renders migrated asset references in formatted output", async () => {
    const result = JSON.stringify([
      { type: "input_text", text: "Before" },
      { type: "agentsview_image", version: 1, text: "![first](asset://first)" },
      { type: "agentsview_image", version: 1, text: "![second](asset://nested/second)" },
      { type: "text", text: "After" },
    ]);
    const base = document.createElement("base");
    base.href = "http://localhost/app/";
    document.head.appendChild(base);

    const toolCall: ToolCall = {
      tool_name: "view_image",
      category: "Other",
      result_content: result,
    };
    await render({ toolCall });
    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();
    document.querySelector<HTMLButtonElement>(".output-header")!.click();
    await tick();
    document.querySelector<HTMLButtonElement>(".output-mode button:nth-child(2)")!.click();
    await tick();

    expect(
      Array.from(document.querySelectorAll<HTMLImageElement>(".formatted-output img"), (img) =>
        img.getAttribute("src"),
      ),
    ).toEqual(["/app/api/v1/assets/first", "/app/api/v1/assets/nested/second"]);
    base.remove();
  });

  it("preserves long path metadata in the accessible text and title", async () => {
    const path = "/home/person/project/" + "very-long-directory/".repeat(6) + "src/example.ts";
    await render({ toolCall: call("Read", { file_path: path }) });
    await click(".tool-header");
    expect(text(".tool-meta")).toContain(path);
    expect(document.querySelector(`.meta-value[title="${path}"]`)).not.toBeNull();
  });
  it("keeps canonical path metadata accessible without labelling ordinary metadata", async () => {
    const path =
      "/workspace/packages/agentsview/frontend/src/lib/components/content/ToolBlock.svelte";
    await render({ toolCall: call("Read", { file_path: path }, { category: "Read" }) });
    await click(".tool-header");
    const file = document.querySelector(".meta-tag");
    expect(file?.textContent).toContain("content/ToolBlock.svelte");
    expect(file?.querySelector(".kit-sr-only")?.textContent).toBe(path);
    expect(file?.querySelector("[aria-label]")).toBeNull();
    expect(document.querySelector(".meta-tag:nth-child(2) [aria-label]")).toBeNull();
  });
  it("localizes result event metadata labels for status, source, and agent", async () => {
    setLocale("zh-CN");
    await render({
      toolCall: call(
        "wait",
        {},
        {
          category: "Other",
          result_content: "latest summary",
          result_events: [
            {
              source: "wait_output",
              status: "completed",
              content: "Finished successfully",
              content_length: 21,
              agent_id: "agent-1",
              event_index: 0,
            },
          ],
        },
      ),
    });
    await click(".tool-header");
    await click(".history-header");
    expect(text(".output-header .output-label")).toBe("输出");
    expect(text(".history-header .output-label")).toBe("历史");
    expect(
      Array.from(document.querySelectorAll(".meta-label"), (node) => node.textContent),
    ).toEqual(["状态：", "来源：", "agent："]);
  });
  it("renders chronological result history with status, source and agent metadata", async () => {
    const result_events = [
      {
        event_index: 4,
        status: "running",
        source: "wait",
        agent_id: "a",
        content: "first event",
        content_length: 11,
      },
      {
        event_index: 9,
        status: "completed",
        source: "wait",
        agent_id: "b",
        content: "last event",
        content_length: 10,
      },
    ];
    await render({ toolCall: call("Task", {}, { result_events }) });
    await click(".tool-header");
    expect(document.querySelector(".result-history")).toBeNull();
    expect(text(".history-header .tool-preview")).toContain("completed: last event");
    await click(".history-header");
    expect(
      Array.from(document.querySelectorAll(".history-content"), (node) => node.textContent),
    ).toEqual(["first event", "last event"]);
    expect(text(".result-history")).toContain("running");
    expect(text(".result-history")).toContain("completed");
    expect(text(".result-history")).toContain("wait");
    expect(text(".result-history")).toContain("a");
    await click(".history-header");
    expect(document.querySelector(".result-history")).toBeNull();
  });
  it("localizes input, output, history and line controls without translating content", async () => {
    setLocale("zh-CN");
    await render({
      content: longCommand,
      toolCall: call(
        "Bash",
        {},
        {
          result_content: "Do not translate output",
          result_events: [
            {
              event_index: 0,
              status: "completed",
              source: "wait",
              content: "Do not translate history",
              content_length: 24,
            },
          ],
        },
      ),
    });
    expect(document.querySelector('button[aria-label="复制输入"]')).not.toBeNull();
    await click(".tool-header");
    expect(text(".output-label")).toContain("输出");
    expect(text(".show-more-btn")).toContain("30");
    expect(text(".show-more-btn")).not.toContain("Show");
    await click(".output-header");
    expect(text(".output-content")).toBe("Do not translate output");
    expect(document.querySelector('button[aria-label="复制输出"]')).not.toBeNull();
    await click(".history-header");
    expect(text(".history-content")).toBe("Do not translate history");
    expect(text(".history-header")).not.toContain("History");
  });
});

describe("ToolBlock input source and copy", () => {
  it("copies a complete task prompt without expanding", async () => {
    const prompt = "Work on this task\nwith all details.";
    await render({ toolCall: call("Task", { subagent_type: "Explore", prompt }) });
    await click('button[aria-label="Copy input"]');
    expect(copyToClipboardMock).toHaveBeenCalledWith(prompt);
    expect(document.querySelector(".tool-content")).toBeNull();
  });
  it("copies all 230 Bash lines before show-all expansion", async () => {
    const command = Array.from({ length: 230 }, (_, i) => `echo hidden-line-${i}`).join("\n");
    await render({ toolCall: call("Bash", { command }, { category: "Bash" }) });
    await click(".tool-header");
    expect(text(".tool-content")).not.toContain("hidden-line-229");
    await click('button[aria-label="Copy input"]');
    expect(copyToClipboardMock).toHaveBeenCalledWith(`command: ${command}`);
    expect(text(".tool-content")).not.toContain("hidden-line-229");
  });
  it.each([
    [
      "custom Edit category",
      call(
        "mcp__custom__edit",
        { old_string: "before", new_string: "after" },
        { category: "Edit" },
      ),
      "-before",
      "+after",
    ],
    [
      "Write category",
      call("write_file", { content: "first\nsecond" }, { category: "Write" }),
      "+first",
      "+second",
    ],
    ["empty Write", call("Write", { content: "" }, { category: "Write" }), "empty file", ""],
    [
      "apply patch fallback",
      call("apply_patch", { patch: "*** Begin Patch\n+new\n*** End Patch" }),
      "Begin Patch",
      "+new",
    ],
    ["path without category", call("Read", { file_path: "/tmp/file.txt" }), "/tmp/file.txt", ""],
    [
      "path with empty category",
      call("Read", { file_path: "/tmp/file.txt" }, { category: "" }),
      "/tmp/file.txt",
      "",
    ],
    [
      "Cursor ApplyPatch diff",
      call(
        "ApplyPatch",
        { diff: "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-before\n+after" },
        { category: "Edit" },
      ),
      "-before",
      "+after",
    ],
  ] as const)("renders %s from the shared source", async (_name, toolCall, first, second) => {
    await render({ toolCall });
    await click(".tool-header");
    const body = text(".diff-view") || text(".tool-content");
    expect(body).toContain(first);
    if (second) expect(body).toContain(second);
  });
  it("prefers supplied content over synthesized input", async () => {
    await render({ content: "explicit source", toolCall: call("Bash", { command: "fallback" }) });
    await click(".tool-header");
    expect(text(".tool-content")).toBe("explicit source");
  });
  it("handles an absent or invalid input_json", async () => {
    await render({ content: "legacy", toolCall: { tool_name: "Read", input_json: "{" } });
    await click(".tool-header");
    expect(text(".tool-content")).toBe("legacy");
  });
  it("handles a tool call without input_json", async () => {
    await render({ toolCall: { tool_name: "Read" } });
    await click(".tool-header");
    expect(document.querySelector(".tool-content")).toBeNull();
  });
  it("handles legacy content without a structured call", async () => {
    await render({ content: "legacy source" });
    await click(".tool-header");
    expect(text(".tool-content")).toBe("legacy source");
  });
  it("truncates long input until Show all and supports Show less", async () => {
    await render({ toolCall: call("Bash", { command: longCommand }, { category: "Bash" }) });
    await click(".tool-header");
    expect(text(".tool-content")).not.toContain("hidden-line-29");
    expect(text(".show-more-btn")).toContain("30");
    await click(".show-more-btn");
    expect(text(".tool-content")).toContain("hidden-line-29");
    expect(text(".show-more-btn").toLowerCase()).toContain("show less");
    await click(".show-more-btn");
    expect(text(".tool-content")).not.toContain("hidden-line-29");
  });
  it("does not offer Show all for short input", async () => {
    await render({ content: "one\ntwo" });
    await click(".tool-header");
    expect(document.querySelector(".show-more-btn")).toBeNull();
  });
  it("reveals hidden Bash input only for the current block", async () => {
    searchState.active = true;
    searchState.current = "7:tool-input:0";
    searchState.query = "hidden-line-29";
    await render({
      toolCall: call("Bash", { command: longCommand }, { category: "Bash" }),
      searchScope: { ordinal: 7, callIdx: 0 },
    });
    expect(document.querySelector(".tool-header")?.getAttribute("aria-expanded")).toBe("true");
    expect(text(".tool-content")).toContain("hidden-line-29");
    expect(text(".show-more-btn").toLowerCase()).toContain("show less");
    expect(document.querySelector("mark")).toBeNull();
  });
});

describe("ToolBlock collapsed previews", () => {
  it.each([
    ["Codex cmd", call("Bash", { cmd: "ls -la" }), "ls -la"],
    ["Claude command", call("Bash", { command: "git status" }), "git status"],
    ["multiline command", call("Bash", { command: "echo first\necho second" }), "echo first"],
    [
      "Edit counts",
      call("Edit", { file_path: "/tmp/test.ts", old_string: "a\nb", new_string: "c\nd\ne" }),
      "test.ts",
    ],
    [
      "TodoWrite in progress",
      call("TodoWrite", {
        todos: [
          { content: "pending item", status: "pending" },
          { content: "active item", status: "in_progress" },
        ],
      }),
      "active item",
    ],
    [
      "TodoWrite final item",
      call("TodoWrite", {
        todos: [
          { content: "first", status: "completed" },
          { content: "last", status: "completed" },
        ],
      }),
      "last",
    ],
    [
      "TaskCreate subject",
      call("TaskCreate", { subject: "Create release", description: "details" }),
      "Create release",
    ],
    [
      "TaskUpdate subject",
      call("TaskUpdate", { taskId: "1", status: "completed", subject: "Ship release" }),
      "Ship release",
    ],
    ["TaskUpdate status", call("TaskUpdate", { taskId: "1", status: "completed" }), "completed"],
    ["Skill", call("Skill", { skill: "my-skill" }), "my-skill"],
    ["lowercase skill", call("skill", { name: "my-skill" }), "my-skill"],
    ["ToolSearch", call("ToolSearch", { query: "select:Read" }), "select:Read"],
    [
      "Task description",
      call("Task", { description: "Explore source", prompt: "Long prompt" }),
      "Explore source",
    ],
    [
      "Task prompt fallback",
      call("Task", { prompt: "Prompt first line\nsecond line" }),
      "Prompt first line",
    ],
    [
      "Agent",
      call("Agent", { description: "Inspect implementation", prompt: "details" }),
      "Inspect implementation",
    ],
    [
      "Zencoder subagent",
      call("subagent", { description: "Explore project", prompt: "details" }, { category: "Task" }),
      "Explore project",
    ],
  ] as const)("preserves %s summary", async (_name, toolCall, expected) => {
    await render({ toolCall });
    expect(text(".tool-header .tool-preview")).toContain(expected);
  });
  it("prefers the structured summary to display content and retains it when expanded", async () => {
    await render({
      content: "legacy misleading preview",
      toolCall: call("Bash", { command: "git status" }),
    });
    expect(text(".tool-preview")).toContain("git status");
    expect(text(".tool-preview")).not.toContain("misleading");
    await click(".tool-header");
    expect(text(".tool-preview")).toContain("git status");
  });
  it("limits a multiline command preview to its first line", async () => {
    await render({ toolCall: call("Bash", { command: "echo first\necho second" }) });
    expect(text(".tool-preview")).toContain("echo first");
    expect(text(".tool-preview")).not.toContain("echo second");
  });
  it("shows legacy preview only while collapsed", async () => {
    await render({ content: "legacy first\nlegacy second" });
    expect(text(".tool-preview")).toBe("legacy first");
    await click(".tool-header");
    expect(document.querySelector(".tool-preview")).toBeNull();
  });
  it("prefers structured TodoWrite summaries to legacy content", async () => {
    await render({
      content: "legacy todo",
      toolCall: call("TodoWrite", { todos: [{ content: "active task", status: "in_progress" }] }),
    });
    expect(text(".tool-preview")).toContain("active task");
    expect(text(".tool-preview")).not.toContain("legacy todo");
  });
});
