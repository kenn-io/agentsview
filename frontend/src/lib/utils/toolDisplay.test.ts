import { describe, it, expect } from "vitest";
import { displayToolName } from "./toolDisplay.js";

describe("displayToolName", () => {
  it("returns category for codex exec_command", () => {
    expect(
      displayToolName({ tool_name: "exec_command", category: "Bash" }),
    ).toBe("Bash");
  });

  it("returns category for Claude Bash", () => {
    expect(
      displayToolName({ tool_name: "Bash", category: "Bash" }),
    ).toBe("Bash");
  });

  it("returns category for codex apply_patch (Edit)", () => {
    expect(
      displayToolName({ tool_name: "apply_patch", category: "Edit" }),
    ).toBe("Edit");
  });

  it("returns tool_name when category is Other", () => {
    expect(
      displayToolName({ tool_name: "weird_tool", category: "Other" }),
    ).toBe("weird_tool");
  });

  it("returns tool_name when category is Tool (skills/MCP)", () => {
    expect(
      displayToolName({ tool_name: "Skill", category: "Tool" }),
    ).toBe("Skill");
  });

  it("returns tool_name when category is missing", () => {
    expect(displayToolName({ tool_name: "Read" })).toBe("Read");
  });

  it("returns tool_name when category is null", () => {
    expect(
      displayToolName({ tool_name: "Read", category: null }),
    ).toBe("Read");
  });

  it("returns tool_name when category is empty string", () => {
    expect(
      displayToolName({ tool_name: "Read", category: "" }),
    ).toBe("Read");
  });
});
