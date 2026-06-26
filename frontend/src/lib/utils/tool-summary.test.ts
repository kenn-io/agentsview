// ABOUTME: Table-driven unit tests for summarizeToolCall.
import { describe, it, expect } from "vite-plus/test";
import type { ToolCall } from "../api/types.js";
import { summarizeToolCall } from "./tool-summary.js";

function call(partial: Partial<ToolCall>): ToolCall {
  return { tool_name: "Tool", ...partial };
}

describe("summarizeToolCall", () => {
  it("returns null for malformed input_json", () => {
    expect(
      summarizeToolCall(
        call({ tool_name: "Read", category: "Read", input_json: "{not json" }),
      ),
    ).toBeNull();
  });

  it("returns null when input_json is absent", () => {
    expect(summarizeToolCall(call({ tool_name: "Read", category: "Read" }))).toBeNull();
  });

  it("returns null when no structured fields are present", () => {
    expect(
      summarizeToolCall(
        call({ tool_name: "mystery", input_json: JSON.stringify({ foo: 1 }) }),
      ),
    ).toBeNull();
  });

  describe("Bash", () => {
    it("shows the command", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Bash",
            category: "Bash",
            input_json: JSON.stringify({ command: "git status" }),
          }),
        ),
      ).toBe("$ git status");
    });

    it("reads the codex cmd key", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "exec_command",
            category: "Bash",
            input_json: JSON.stringify({ cmd: "ls -la" }),
          }),
        ),
      ).toBe("$ ls -la");
    });

    it("uses only the first line of a multiline command", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Bash",
            category: "Bash",
            input_json: JSON.stringify({ command: "cat <<EOF\nhello\nEOF" }),
          }),
        ),
      ).toBe("$ cat <<EOF");
    });

    it("never adds a suffix for Bash", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Bash",
            category: "Bash",
            input_json: JSON.stringify({ command: "ls" }),
            result_content: "a\nb\nc",
          }),
        ),
      ).toBe("$ ls");
    });
  });

  describe("Read", () => {
    it("appends a line count from result_content", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Read",
            category: "Read",
            input_json: JSON.stringify({ file_path: "README.md" }),
            result_content: "line one\nline two",
          }),
        ),
      ).toBe("README.md (2 lines)");
    });

    it("counts a blank line inside the file", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Read",
            category: "Read",
            input_json: JSON.stringify({ file_path: "a.txt" }),
            result_content: "one\n\nthree",
          }),
        ),
      ).toBe("a.txt (3 lines)");
    });

    it("ignores a single trailing newline", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Read",
            category: "Read",
            input_json: JSON.stringify({ file_path: "a.txt" }),
            result_content: "one\ntwo\n",
          }),
        ),
      ).toBe("a.txt (2 lines)");
    });

    it("omits the suffix when result_content is missing", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Read",
            category: "Read",
            input_json: JSON.stringify({ file_path: "README.md" }),
          }),
        ),
      ).toBe("README.md");
    });

    it("returns null when no file path is present", () => {
      expect(
        summarizeToolCall(
          call({ tool_name: "Read", category: "Read", input_json: JSON.stringify({}) }),
        ),
      ).toBeNull();
    });

    it("omits the suffix when the file reads as zero lines", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Read",
            category: "Read",
            input_json: JSON.stringify({ file_path: "a.txt" }),
            result_content: "\n",
          }),
        ),
      ).toBe("a.txt");
    });
  });

  describe("Edit", () => {
    it("shows +added -removed line counts", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Edit",
            category: "Edit",
            input_json: JSON.stringify({
              file_path: "main.go",
              old_string: "a\nb",
              new_string: "a\nb\nc\nd",
            }),
          }),
        ),
      ).toBe("main.go (+4 -2)");
    });

    it("counts a single-line replacement as +1 -1", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Edit",
            category: "Edit",
            input_json: JSON.stringify({
              file_path: "main.go",
              old_string: "old",
              new_string: "new",
            }),
          }),
        ),
      ).toBe("main.go (+1 -1)");
    });

    it("treats an empty old_string (insertion) as -0", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Edit",
            category: "Edit",
            input_json: JSON.stringify({
              file_path: "main.go",
              old_string: "",
              new_string: "added",
            }),
          }),
        ),
      ).toBe("main.go (+1 -0)");
    });

    it("omits counts for a patch-only Edit input", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "ApplyPatch",
            category: "Edit",
            input_json: JSON.stringify({ path: "src/app.ts", patch: "@@ -1 +1 @@" }),
          }),
        ),
      ).toBe("src/app.ts");
    });

    it("returns null when no file field is present", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "apply_patch",
            category: "Edit",
            input_json: JSON.stringify({ patch_file: "/p.diff" }),
          }),
        ),
      ).toBeNull();
    });

    it("omits the count for a no-op (both strings empty)", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Edit",
            category: "Edit",
            input_json: JSON.stringify({
              file_path: "main.go",
              old_string: "",
              new_string: "",
            }),
          }),
        ),
      ).toBe("main.go");
    });
  });

  describe("Write", () => {
    it("shows the +N content line count", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Write",
            category: "Write",
            input_json: JSON.stringify({ file_path: "out.txt", content: "a\nb\nc" }),
          }),
        ),
      ).toBe("out.txt (+3)");
    });

    it("ignores a single trailing newline in content", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Write",
            category: "Write",
            input_json: JSON.stringify({ file_path: "out.txt", content: "a\nb\n" }),
          }),
        ),
      ).toBe("out.txt (+2)");
    });

    it("omits the suffix when content is absent", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Write",
            category: "Write",
            input_json: JSON.stringify({ file_path: "out.txt" }),
          }),
        ),
      ).toBe("out.txt");
    });

    it("omits the suffix when content is an empty string", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Write",
            category: "Write",
            input_json: JSON.stringify({ file_path: "out.txt", content: "" }),
          }),
        ),
      ).toBe("out.txt");
    });
  });

  describe("Grep", () => {
    it("counts matches from newline-separated output", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Grep",
            category: "Grep",
            input_json: JSON.stringify({ pattern: "TODO" }),
            result_content: "a.go:1:TODO\nb.go:2:TODO\nc.go:9:TODO",
          }),
        ),
      ).toBe("TODO (3 matches)");
    });

    it("omits the count when output_mode is count", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Grep",
            category: "Grep",
            input_json: JSON.stringify({ pattern: "TODO", output_mode: "count" }),
            result_content: "a.go:4\nb.go:2",
          }),
        ),
      ).toBe("TODO");
    });

    it("omits the count for blank/noisy output", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Grep",
            category: "Grep",
            input_json: JSON.stringify({ pattern: "TODO" }),
            result_content: "\n\n",
          }),
        ),
      ).toBe("TODO");
    });

    it("omits the count when result_content is missing", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Grep",
            category: "Grep",
            input_json: JSON.stringify({ pattern: "TODO" }),
          }),
        ),
      ).toBe("TODO");
    });
  });

  describe("Glob", () => {
    it("counts files from newline-separated output", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Glob",
            category: "Glob",
            input_json: JSON.stringify({ pattern: "**/*.ts" }),
            result_content: "a.ts\nb.ts",
          }),
        ),
      ).toBe("**/*.ts (2 files)");
    });

    it("omits the count for blank output", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Glob",
            category: "Glob",
            input_json: JSON.stringify({ pattern: "**/*.ts" }),
            result_content: "   ",
          }),
        ),
      ).toBe("**/*.ts");
    });
  });

  describe("special tools", () => {
    it("summarizes the in-progress todo", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "TodoWrite",
            input_json: JSON.stringify({
              todos: [
                { content: "done", status: "completed" },
                { content: "now", status: "in_progress" },
              ],
            }),
          }),
        ),
      ).toBe("→ now");
    });

    it("falls back to the last todo when none are in progress", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "TodoWrite",
            input_json: JSON.stringify({
              todos: [
                { content: "one", status: "completed" },
                { content: "two", status: "completed" },
              ],
            }),
          }),
        ),
      ).toBe("→ two");
    });

    it("summarizes a TaskUpdate", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "TaskUpdate",
            input_json: JSON.stringify({
              taskId: 29,
              status: "in_progress",
              subject: "Rebuild",
            }),
          }),
        ),
      ).toBe("#29 · in_progress · Rebuild");
    });

    it("summarizes a Skill by name", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Skill",
            input_json: JSON.stringify({ skill: "review-branch" }),
          }),
        ),
      ).toBe("review-branch");
    });

    it("summarizes a Task by description", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "Task",
            category: "Task",
            input_json: JSON.stringify({
              subagent_type: "Explore",
              description: "Explore the repo",
            }),
          }),
        ),
      ).toBe("Explore the repo");
    });
  });

  describe("generic structured fallback", () => {
    it("shows a known file path for an unrecognized category", () => {
      expect(
        summarizeToolCall(
          call({
            tool_name: "custom",
            category: "Other",
            input_json: JSON.stringify({ file_path: "/etc/hosts" }),
          }),
        ),
      ).toBe("/etc/hosts");
    });
  });
});
