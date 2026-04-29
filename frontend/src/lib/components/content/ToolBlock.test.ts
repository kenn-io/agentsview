// @vitest-environment jsdom
// ABOUTME: Unit tests for ToolBlock's output section behavior.
// ABOUTME: Covers visibility, collapse/expand, and preview of result_content.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
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

  it("prefers explicit content over command fallback", async () => {
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
    expect(preview!.textContent).toBe("$ from content");
  });
});
