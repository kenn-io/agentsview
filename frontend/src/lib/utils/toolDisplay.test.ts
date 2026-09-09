import { describe, it, expect } from "vite-plus/test";
import { displayToolName, displayToolResult } from "./toolDisplay.js";

describe("displayToolName", () => {
  it("returns category for codex exec_command", () => {
    expect(displayToolName({ tool_name: "exec_command", category: "Bash" })).toBe("Bash");
  });

  it("returns category for Claude Bash", () => {
    expect(displayToolName({ tool_name: "Bash", category: "Bash" })).toBe("Bash");
  });

  it("returns category for codex apply_patch (Edit)", () => {
    expect(displayToolName({ tool_name: "apply_patch", category: "Edit" })).toBe("Edit");
  });

  it("returns tool_name when category is Other", () => {
    expect(displayToolName({ tool_name: "weird_tool", category: "Other" })).toBe("weird_tool");
  });

  it("returns tool_name when category is Tool (skills/MCP)", () => {
    expect(displayToolName({ tool_name: "Skill", category: "Tool" })).toBe("Skill");
  });

  it("returns tool_name when category is missing", () => {
    expect(displayToolName({ tool_name: "Read" })).toBe("Read");
  });

  it("returns tool_name when category is null", () => {
    expect(displayToolName({ tool_name: "Read", category: null })).toBe("Read");
  });

  it("returns tool_name when category is empty string", () => {
    expect(displayToolName({ tool_name: "Read", category: "" })).toBe("Read");
  });
});

describe("displayToolResult", () => {
  it("preserves grouped summary labels and order while displaying image placeholders", () => {
    const content =
      'agent-b:\n[{"type":"input_text","text":"Before"},{"type":"agentsview_image","version":1,"text":"[Image: image/png, 3 bytes]"}]\n\nagent-a:\n[{"type":"agentsview_image","version":1,"text":"[Image: image/jpeg, 6 bytes]"},{"type":"text","text":"After"}]\n\nagent-c:\n[{"type":"custom","value":42}]\n\n[{"type":"agentsview_image","version":1,"text":"[Image: image/gif, 9 bytes]"}]';
    expect(displayToolResult(content)).toBe(
      'agent-b:\nBefore\n\n[Image: image/png, 3 bytes]\n\nagent-a:\n[Image: image/jpeg, 6 bytes]\n\nAfter\n\nagent-c:\n[{"type":"custom","value":42}]\n\n[Image: image/gif, 9 bytes]',
    );
  });

  it("displays an unlabelled single-agent summary followed by anonymous output", () => {
    const content =
      '[{"type":"agentsview_image","version":1,"text":"[Image: image/png, 3 bytes]"}]\n\nFinished';
    expect(displayToolResult(content)).toBe("[Image: image/png, 3 bytes]\n\nFinished");
  });

  it.each([
    "ordinary output",
    '[{"type":"text","text":"ordinary JSON output"}]',
    '[{"type":"agentsview_image","version":1,"text":"[Image]"},{"type":"input_image","image_url":"https://example.com/image.png"}]',
    '[{"type":"agentsview_image","version":2,"text":"[Image]"}]',
    '[{"type":"agentsview_image","version":1,"text":"[Image]"},{"type":"input_text"}]',
    '[{"type":"agentsview_image",',
  ])("preserves unrecognized result content: %s", (content) => {
    expect(displayToolResult(content)).toBe(content);
  });
});
