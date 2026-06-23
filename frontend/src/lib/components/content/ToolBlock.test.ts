// @vitest-environment jsdom
// ABOUTME: Unit tests for ToolBlock's output section behavior.
// ABOUTME: Covers visibility, collapse/expand, and preview of result_content.
import { describe, it, expect, vi, afterEach } from "vite-plus/test";
import { mount, unmount, tick } from "svelte";
import type { ToolCall } from "../../api/types.js";

vi.mock("./SubagentInline.svelte", () => ({
  default: {},
}));

// @ts-ignore
import ToolBlock from "./ToolBlock.svelte";

describe("ToolBlock output section", () => {
  let component: ReturnType<typeof mount>;

  afterEach(() => {
    if (component) unmount(component);
    document.body.innerHTML = "";
  });

  it("does not render output-header when toolCall has no result_content", async () => {
    const toolCall: ToolCall = {
      tool_name: "Read",
      category: "file",
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "some input", toolCall },
    });
    await tick();

    expect(document.querySelector(".output-header")).toBeNull();
  });

  it("does not render output-header when toolCall is absent", async () => {
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "some input" },
    });
    await tick();

    expect(document.querySelector(".output-header")).toBeNull();
  });

  it("renders output-header after expanding the tool block when result_content is set", async () => {
    const toolCall: ToolCall = {
      tool_name: "Read",
      category: "file",
      result_content: "line one\nline two",
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "some input", toolCall },
    });
    await tick();

    // Output section is inside the collapsed block — not visible yet.
    expect(document.querySelector(".output-header")).toBeNull();

    // Expand the main tool block.
    const toolHeader = document.querySelector<HTMLButtonElement>(".tool-header");
    expect(toolHeader).not.toBeNull();
    toolHeader!.click();
    await tick();

    expect(document.querySelector(".output-header")).not.toBeNull();
  });

  it("output starts collapsed after expanding the tool block", async () => {
    const toolCall: ToolCall = {
      tool_name: "Read",
      category: "file",
      result_content: "line one\nline two",
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "some input", toolCall },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    // Output content pre block should not be present when output is collapsed.
    expect(document.querySelector(".output-content")).toBeNull();
  });

  it("expands output content on clicking output-header", async () => {
    const resultText = "line one\nline two\nline three";
    const toolCall: ToolCall = {
      tool_name: "Read",
      category: "file",
      result_content: resultText,
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "some input", toolCall },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    document.querySelector<HTMLButtonElement>(".output-header")!.click();
    await tick();

    const outputContent = document.querySelector(".output-content");
    expect(outputContent).not.toBeNull();
    expect(outputContent!.textContent).toBe(resultText);
  });

  it("shows first line as preview when output is collapsed", async () => {
    const toolCall: ToolCall = {
      tool_name: "Read",
      category: "file",
      result_content: "first line\nsecond line",
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "some input", toolCall },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    // Output is collapsed — preview should show first line.
    const outputHeader = document.querySelector(".output-header");
    expect(outputHeader).not.toBeNull();
    const preview = outputHeader!.querySelector(".tool-preview");
    expect(preview).not.toBeNull();
    expect(preview!.textContent).toBe("first line");
  });

  it("renders history after expanding the tool block when result_events are set", async () => {
    const toolCall: ToolCall = {
      tool_name: "wait",
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
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "some input", toolCall },
    });
    await tick();

    expect(document.querySelector(".history-header")).toBeNull();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    expect(document.querySelector(".history-header")).not.toBeNull();
  });

  it("expands event history and shows chronological event content", async () => {
    const toolCall: ToolCall = {
      tool_name: "wait",
      category: "Other",
      result_content: "agent-a:\nFirst finished\n\nagent-b:\nSecond finished",
      result_events: [
        {
          source: "wait_output",
          status: "completed",
          content: "First finished",
          content_length: 14,
          agent_id: "agent-a",
          event_index: 0,
        },
        {
          source: "subagent_notification",
          status: "completed",
          content: "Second finished",
          content_length: 15,
          agent_id: "agent-b",
          event_index: 1,
        },
      ],
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "some input", toolCall },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();
    document.querySelector<HTMLButtonElement>(".history-header")!.click();
    await tick();

    const historyEntries = Array.from(document.querySelectorAll(".history-content"));
    expect(historyEntries).toHaveLength(2);
    expect(historyEntries[0]!.textContent).toBe("First finished");
    expect(historyEntries[1]!.textContent).toBe("Second finished");
  });
});

describe("ToolBlock fallback content", () => {
  let component: ReturnType<typeof mount>;

  afterEach(() => {
    if (component) unmount(component);
    document.body.innerHTML = "";
  });

  it("renders fallback content when content is empty and category matches", async () => {
    // Edit category should show file path from input_json
    const toolCall: ToolCall = {
      tool_name: "custom_edit",
      category: "Edit",
      input_json: JSON.stringify({ file_path: "/path/to/file.txt" }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", toolCall },
    });
    await tick();

    // Expand to see content
    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    const toolContent = document.querySelector(".tool-content");
    expect(toolContent).not.toBeNull();
    expect(toolContent!.textContent).toContain("file_path: /path/to/file.txt");
  });

  it("renders fallback content for Write tools", async () => {
    const toolCall: ToolCall = {
      tool_name: "custom_write",
      category: "Write",
      input_json: JSON.stringify({ file_path: "/output.txt", content: "Hello World" }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", toolCall },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    const diffView = document.querySelector(".diff-view");
    expect(diffView).not.toBeNull();
    expect(diffView!.textContent).toContain("Hello World");
  });

  it("falls back to tool_name when category has no specific pattern", async () => {
    // apply_patch doesn't match Edit pattern (which expects old_string/new_string)
    // so it should fall back to generic key-value output
    const toolCall: ToolCall = {
      tool_name: "apply_patch",
      category: "Edit",
      input_json: JSON.stringify({ patch_file: "/path/to/patch.diff" }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", toolCall },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    const toolContent = document.querySelector(".tool-content");
    expect(toolContent).not.toBeNull();
    // Should show the generic key-value output with exact format
    expect(toolContent!.textContent).toContain("patch_file: /path/to/patch.diff");
  });

  it("renders Cursor ApplyPatch patch input as a diff", async () => {
    const toolCall: ToolCall = {
      tool_name: "ApplyPatch",
      category: "Edit",
      input_json: JSON.stringify({
        path: "src/app.ts",
        patch: "@@ -1,1 +1,1 @@\n-old\n+new",
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", toolCall },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    const diffView = document.querySelector(".diff-view");
    expect(diffView).not.toBeNull();
    expect(diffView!.textContent).toContain("-old");
    expect(diffView!.textContent).toContain("+new");
  });

  it("renders fallback content when no category is provided", async () => {
    // Tool without category - should use tool_name directly
    const toolCall: ToolCall = {
      tool_name: "apply_patch",
      input_json: JSON.stringify({ patch_file: "/path/to/patch.diff" }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", toolCall },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    const toolContent = document.querySelector(".tool-content");
    expect(toolContent).not.toBeNull();
    // apply_patch doesn't have specific handling, so should show generic output
    expect(toolContent!.textContent).toContain("patch_file: /path/to/patch.diff");
  });

  it("falls back to tool_name when category is empty string", async () => {
    // Empty string category should be treated same as no category
    const toolCall: ToolCall = {
      tool_name: "apply_patch",
      category: "",
      input_json: JSON.stringify({ patch_file: "/path/to/patch.diff" }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", toolCall },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    const toolContent = document.querySelector(".tool-content");
    expect(toolContent).not.toBeNull();
    // Should fall back to tool_name and show generic output
    expect(toolContent!.textContent).toContain("patch_file: /path/to/patch.diff");
  });

  it("does not render fallback content when content is provided", async () => {
    const toolCall: ToolCall = {
      tool_name: "custom_tool",
      input_json: JSON.stringify({ param: "value" }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "Explicit content here", toolCall },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    const toolContent = document.querySelector(".tool-content");
    expect(toolContent).not.toBeNull();
    expect(toolContent!.textContent).toBe("Explicit content here");
  });

  it("does not render fallback content when input_json is empty", async () => {
    const toolCall: ToolCall = {
      tool_name: "custom_tool",
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", toolCall },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    const toolContent = document.querySelector(".tool-content");
    expect(toolContent).toBeNull();
  });

  it("does not render fallback content when no toolCall is provided", async () => {
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "" },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    const toolContent = document.querySelector(".tool-content");
    expect(toolContent).toBeNull();
  });
});

describe("ToolBlock show-more for long content", () => {
  let component: ReturnType<typeof mount>;

  afterEach(() => {
    if (component) unmount(component);
    document.body.innerHTML = "";
  });

  it("shows 'show all' button for long Bash fallback content", async () => {
    const longCommand = Array.from({ length: 30 }, (_, i) => `echo line${i}`).join("\n");
    const toolCall: ToolCall = {
      tool_name: "Bash",
      category: "Bash",
      input_json: JSON.stringify({ command: longCommand }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", toolCall },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    const showMoreBtn = document.querySelector(".show-more-btn");
    expect(showMoreBtn).not.toBeNull();
    expect(showMoreBtn!.textContent).toContain("show all");
  });

  it("expands to full content when 'show all' is clicked", async () => {
    const longCommand = Array.from({ length: 30 }, (_, i) => `echo line${i}`).join("\n");
    const toolCall: ToolCall = {
      tool_name: "Bash",
      category: "Bash",
      input_json: JSON.stringify({ command: longCommand }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", toolCall },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    const contentBefore = document.querySelector(".tool-content")!.textContent!;
    expect(contentBefore).not.toContain("echo line29");

    document.querySelector<HTMLButtonElement>(".show-more-btn")!.click();
    await tick();

    const contentAfter = document.querySelector(".tool-content")!.textContent!;
    expect(contentAfter).toContain("echo line29");

    const showMoreBtn = document.querySelector(".show-more-btn");
    expect(showMoreBtn!.textContent).toContain("show less");
  });

  it("does not show 'show all' button for short content", async () => {
    const toolCall: ToolCall = {
      tool_name: "Bash",
      category: "Bash",
      input_json: JSON.stringify({ command: "npm test" }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", toolCall },
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    expect(document.querySelector(".show-more-btn")).toBeNull();
  });

  it("auto-expands hidden Bash fallback content on search match", async () => {
    const longCommand = Array.from(
      { length: 30 },
      (_, i) => `echo hidden-line-${i}`,
    ).join("\n");
    const toolCall: ToolCall = {
      tool_name: "Bash",
      category: "Bash",
      input_json: JSON.stringify({ command: longCommand }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: {
        content: "",
        toolCall,
        highlightQuery: "hidden-line-29",
      },
    });
    await tick();

    const toolContent = document.querySelector(".tool-content");
    expect(toolContent).not.toBeNull();
    expect(toolContent!.textContent).toContain("hidden-line-29");
    expect(document.querySelector(".show-more-btn")!.textContent).toContain(
      "show less",
    );
  });
});

describe("ToolBlock collapsed preview", () => {
  let component: ReturnType<typeof mount>;

  afterEach(() => {
    if (component) unmount(component);
    document.body.innerHTML = "";
  });

  it("shows codex bash command (cmd key) when content is empty", async () => {
    const toolCall: ToolCall = {
      tool_name: "exec_command",
      category: "Bash",
      input_json: JSON.stringify({
        cmd: "nl -ba file.md",
        workdir: "/x",
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "Bash", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview).not.toBeNull();
    expect(preview!.textContent).toBe("$ nl -ba file.md");
  });

  it("shows claude bash command (command key) when content is empty", async () => {
    const toolCall: ToolCall = {
      tool_name: "Bash",
      category: "Bash",
      input_json: JSON.stringify({ command: "ls -la" }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "Bash", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview).not.toBeNull();
    expect(preview!.textContent).toBe("$ ls -la");
  });

  it("shows only the first line of multi-line bash commands", async () => {
    const toolCall: ToolCall = {
      tool_name: "exec_command",
      category: "Bash",
      input_json: JSON.stringify({
        cmd: "cat <<EOF\nhello\nworld\nEOF",
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "Bash", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview).not.toBeNull();
    expect(preview!.textContent).toBe("$ cat <<EOF");
  });

  it("prefers the structured command summary over display content", async () => {
    const toolCall: ToolCall = {
      tool_name: "exec_command",
      category: "Bash",
      input_json: JSON.stringify({ cmd: "from json" }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "$ from content", label: "Bash", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview!.textContent).toBe("$ from json");
  });

  it("keeps the structured summary visible after expanding (ungated)", async () => {
    const toolCall: ToolCall = {
      tool_name: "Read",
      category: "Read",
      input_json: JSON.stringify({ file_path: "README.md" }),
      result_content: "line one\nline two",
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "Read", toolCall },
    });
    await tick();

    const before = document.querySelector(".tool-header .tool-preview");
    expect(before!.textContent).toBe("README.md (2 lines)");

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    const after = document.querySelector(".tool-header .tool-preview");
    expect(after).not.toBeNull();
    expect(after!.textContent).toBe("README.md (2 lines)");
  });

  it("shows the +added -removed suffix for an Edit", async () => {
    const toolCall: ToolCall = {
      tool_name: "Edit",
      category: "Edit",
      input_json: JSON.stringify({
        file_path: "main.go",
        old_string: "a",
        new_string: "a\nb\nc",
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "Edit", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview!.textContent).toBe("main.go (+3 -1)");
  });

  it("keeps the legacy first-line preview collapsed-only", async () => {
    // "mystery" with no recognized fields makes summarizeToolCall return
    // null, so the legacy content-first-line preview is the only thing
    // rendered — and it must stay gated on the collapsed state.
    const toolCall: ToolCall = {
      tool_name: "mystery",
      input_json: JSON.stringify({ foo: 1 }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "plain first line", label: "mystery", toolCall },
    });
    await tick();

    const collapsed = document.querySelector(".tool-header .tool-preview");
    expect(collapsed!.textContent).toBe("plain first line");

    document.querySelector<HTMLButtonElement>(".tool-header")!.click();
    await tick();

    expect(document.querySelector(".tool-header .tool-preview")).toBeNull();
  });

  it("shows in-progress todo content for TodoWrite", async () => {
    const toolCall: ToolCall = {
      tool_name: "TodoWrite",
      input_json: JSON.stringify({
        todos: [
          { content: "first done task", status: "completed" },
          { content: "current work", status: "in_progress" },
          { content: "future task", status: "pending" },
        ],
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "TodoWrite", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview).not.toBeNull();
    expect(preview!.textContent).toBe("→ current work");
  });

  it("falls back to last todo when none are in-progress", async () => {
    const toolCall: ToolCall = {
      tool_name: "TodoWrite",
      input_json: JSON.stringify({
        todos: [
          { content: "task one", status: "completed" },
          { content: "task two", status: "completed" },
        ],
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "TodoWrite", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview!.textContent).toBe("→ task two");
  });

  it("prefers TodoWrite synthesis over content first line", async () => {
    const toolCall: ToolCall = {
      tool_name: "TodoWrite",
      input_json: JSON.stringify({
        todos: [{ content: "do thing", status: "in_progress" }],
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: {
        content: "[Todo List]\n  → do thing",
        label: "TodoWrite",
        toolCall,
      },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview!.textContent).toBe("→ do thing");
  });

  it("shows subject for TaskCreate", async () => {
    const toolCall: ToolCall = {
      tool_name: "TaskCreate",
      input_json: JSON.stringify({
        subject: "Rebuild Companies list table columns",
        description: "long description here",
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "TaskCreate", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview!.textContent).toBe("Rebuild Companies list table columns");
  });

  it("shows task id, status, and subject for TaskUpdate", async () => {
    const toolCall: ToolCall = {
      tool_name: "TaskUpdate",
      input_json: JSON.stringify({
        taskId: 29,
        status: "in_progress",
        subject: "Rebuild Companies list table columns",
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "TaskUpdate", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview!.textContent).toBe(
      "#29 · in_progress · Rebuild Companies list table columns",
    );
  });

  it("shows just task id and status for TaskUpdate without subject", async () => {
    const toolCall: ToolCall = {
      tool_name: "TaskUpdate",
      input_json: JSON.stringify({
        taskId: 29,
        status: "completed",
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "TaskUpdate", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview!.textContent).toBe("#29 · completed");
  });

  it("shows skill name for Skill tool", async () => {
    const toolCall: ToolCall = {
      tool_name: "Skill",
      input_json: JSON.stringify({ skill: "roborev-review-branch" }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "Skill", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview!.textContent).toBe("roborev-review-branch");
  });

  it("shows skill name from name field for lowercase skill tool", async () => {
    const toolCall: ToolCall = {
      tool_name: "skill",
      input_json: JSON.stringify({ name: "my-skill" }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "skill", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview!.textContent).toBe("my-skill");
  });

  it("shows query for ToolSearch", async () => {
    const toolCall: ToolCall = {
      tool_name: "ToolSearch",
      input_json: JSON.stringify({
        query: "select:TaskOutput,TaskGet",
        max_results: 2,
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "ToolSearch", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview!.textContent).toBe("select:TaskOutput,TaskGet");
  });

  it("shows description for Task tool", async () => {
    const toolCall: ToolCall = {
      tool_name: "Task",
      input_json: JSON.stringify({
        subagent_type: "Explore",
        description: "Explore agentsview project",
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "Task", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview!.textContent).toBe("Explore agentsview project");
  });

  it("falls back to prompt when description is missing for Task", async () => {
    const toolCall: ToolCall = {
      tool_name: "Task",
      input_json: JSON.stringify({
        subagent_type: "Explore",
        prompt: "Find the foo function\nand return its location",
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "Task", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview!.textContent).toBe("Find the foo function");
  });

  it("uses Task preview for Agent tool", async () => {
    const toolCall: ToolCall = {
      tool_name: "Agent",
      input_json: JSON.stringify({
        description: "Audit ship readiness",
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "Agent", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview!.textContent).toBe("Audit ship readiness");
  });

  it("uses Task preview for subagent-style tool names", async () => {
    const toolCall: ToolCall = {
      tool_name: "Zencoder_subagent__ZencoderSubagent",
      input_json: JSON.stringify({
        description: "Run subagent task",
      }),
    };
    component = mount(ToolBlock, {
      target: document.body,
      props: { content: "", label: "subagent", toolCall },
    });
    await tick();

    const preview = document.querySelector(".tool-header .tool-preview");
    expect(preview!.textContent).toBe("Run subagent task");
  });
});
