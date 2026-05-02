import { describe, it, expect } from "vitest";
import {
  parseContent,
  isToolOnly,
  enrichSegments,
  hasVisibleSegments,
} from "./content-parser.js";
import type { Message, ToolCall } from "../api/types.js";

let nextId = 1;

function makeMsg(
  overrides: Partial<Message> & { content: string },
): Message {
  const defaults: Message = {
    id: nextId++,
    session_id: "s1",
    ordinal: 0,
    role: "assistant",
    content: "",
    has_tool_use: false,
    has_thinking: false,
    thinking_text: "",
    content_length: 0,
    model: "",
    token_usage: null,
    context_tokens: 0,
    output_tokens: 0,
    timestamp: "2024-01-01T00:00:00Z",
    is_system: false,
  };
  return { ...defaults, ...overrides };
}

describe("parseContent", () => {
  it("returns empty array for empty string", () => {
    expect(parseContent("")).toEqual([]);
  });

  it("preserves leading whitespace in plain text", () => {
    const segments = parseContent("  - Indented list item");
    expect(segments).toEqual([
      { type: "text", content: "  - Indented list item" },
    ]);
  });

  it("removes trailing whitespace in plain text", () => {
    const segments =
      parseContent("Text with trailing space   \n");
    expect(segments).toEqual([
      { type: "text", content: "Text with trailing space" },
    ]);
  });

  it("keeps blockquote markers in prose as one text segment", () => {
    const content =
      "blabla1\n\n> blabla2\n\nblabla3\n\n> blabla4\n\nblabla5";
    expect(parseContent(content)).toEqual([
      { type: "text", content },
    ]);
  });

  it("preserves leading whitespace before blocks", () => {
    const segments =
      parseContent("  Indented text\n[Thinking]\n...");
    expect(segments[0]).toEqual({
      type: "text",
      content: "  Indented text",
    });
    expect(segments[1]).toMatchObject({ type: "thinking" });
  });

  it("handles whitespace in gaps between blocks", () => {
    const text = "[Thinking]\nfoo\n[Bash]\necho hi";
    const segments = parseContent(text);
    expect(segments.map((s) => s.type)).toEqual([
      "thinking",
      "tool",
    ]);
  });

  it("preserves leading whitespace in tail text", () => {
    const segments =
      parseContent("```code\ncontent```\n  Trailing text");
    expect(segments).toHaveLength(2);
    expect(segments[0]).toMatchObject({ type: "code" });
    expect(segments[1]).toEqual({
      type: "text",
      content: "\n  Trailing text",
    });
  });

  it("skips code blocks inside tool blocks", () => {
    const text =
      "[Bash]\n```sh\necho hi\n```\n\nsome text after";
    const segments = parseContent(text);
    const types = segments.map((s) => s.type);
    expect(types).not.toContain("code");
    expect(types).toContain("tool");
  });

  it("extracts code block language as label", () => {
    const segments =
      parseContent("```typescript\nconst x = 1;\n```");
    expect(segments).toEqual([
      {
        type: "code",
        content: "const x = 1;\n",
        label: "typescript",
      },
    ]);
  });

  it("omits label for code blocks without language", () => {
    const segments = parseContent("```\nplain code\n```");
    expect(segments[0]).toEqual({
      type: "code",
      content: "plain code\n",
      label: undefined,
    });
  });

  it("extracts tool name and args as label", () => {
    const segments = parseContent("[Read /foo/bar.ts]\nfile");
    expect(segments[0]).toEqual({
      type: "tool",
      content: "file",
      label: "Read /foo/bar.ts",
    });
  });

  it("parses codex tool markers and normalizes label", () => {
    const segments = parseContent("[exec_command]\n$ rg --files");
    expect(segments[0]).toEqual({
      type: "tool",
      content: "$ rg --files",
      label: "Bash",
    });
  });

  it("drops overlapping matches", () => {
    const text = "[Thinking]\nI think\n[Bash]\necho ok";
    const segments = parseContent(text);
    // Both blocks should parse without overlap
    expect(segments.map((s) => s.type)).toEqual([
      "thinking",
      "tool",
    ]);
  });

  it("returns consistent results on repeated calls", () => {
    const text = "Hello world";
    const first = parseContent(text);
    const second = parseContent(text);
    expect(first).toEqual(second);
  });
});

describe("parseContent - thinking blocks", () => {
  it("separates thinking from following text at blank line", () => {
    const text =
      "[Thinking]\nsome thoughts\n\nHere is my response";
    const segments = parseContent(text, false);
    expect(segments).toHaveLength(2);
    expect(segments[0]).toMatchObject({
      type: "thinking",
      content: "some thoughts",
    });
    expect(segments[1]).toMatchObject({
      type: "text",
      content: "Here is my response",
    });
  });

  it("merges consecutive thinking blocks into one", () => {
    const text =
      "[Thinking]\nfirst thought\n[Thinking]\nsecond thought";
    const segments = parseContent(text, false);
    const thinking = segments.filter(
      (s) => s.type === "thinking",
    );
    expect(thinking).toHaveLength(1);
    expect(thinking[0]!.content).toContain("first thought");
    expect(thinking[0]!.content).toContain("second thought");
  });

  it("does not merge thinking blocks separated by text", () => {
    const text =
      "[Thinking]\nthought one\n\nSome text\n[Thinking]\nthought two";
    const segments = parseContent(text, false);
    const thinking = segments.filter(
      (s) => s.type === "thinking",
    );
    expect(thinking).toHaveLength(2);
  });

  it("shows response text when thinking is stripped", () => {
    const text =
      "[Thinking]\nanalysis\n\nThe answer is 42.";
    const segments = parseContent(text, false);
    const textSegs = segments.filter(
      (s) => s.type === "text",
    );
    expect(textSegs).toHaveLength(1);
    expect(textSegs[0]!.content).toBe("The answer is 42.");
  });

  it("uses [/Thinking] end marker to delimit content", () => {
    const text =
      "[Thinking]\nmy thoughts\n[/Thinking]\n\nResponse text";
    const segments = parseContent(text, false);
    expect(segments).toHaveLength(2);
    expect(segments[0]).toMatchObject({
      type: "thinking",
      content: "my thoughts",
    });
    expect(segments[1]).toMatchObject({
      type: "text",
      content: "Response text",
    });
  });

  it("handles end marker with no blank line before text", () => {
    const text =
      "[Thinking]\nthoughts\n[/Thinking]\n" +
      "[Thinking]\nmore\n[/Thinking]\n\nResponse";
    const segments = parseContent(text, false);
    const thinking = segments.filter(
      (s) => s.type === "thinking",
    );
    expect(thinking).toHaveLength(1);
    expect(thinking[0]!.content).toContain("thoughts");
    expect(thinking[0]!.content).toContain("more");
    const textSegs = segments.filter(
      (s) => s.type === "text",
    );
    expect(textSegs).toHaveLength(1);
    expect(textSegs[0]!.content).toBe("Response");
  });

  it("preserves blank lines inside marked thinking block", () => {
    const text =
      "[Thinking]\npara1\n\npara2\n[/Thinking]";
    const segments = parseContent(text, false);
    expect(segments).toHaveLength(1);
    expect(segments[0]!.type).toBe("thinking");
    expect(segments[0]!.content).toContain("para1");
    expect(segments[0]!.content).toContain("para2");
  });

  it("preserves bracket lines inside marked thinking", () => {
    const text =
      "[Thinking]\nI see [Read: foo] in the code\n" +
      "[/Thinking]\n\nResponse";
    const segments = parseContent(text, false);
    const thinking = segments.filter(
      (s) => s.type === "thinking",
    );
    expect(thinking).toHaveLength(1);
    expect(thinking[0]!.content).toContain("[Read: foo]");
    const textSegs = segments.filter(
      (s) => s.type === "text",
    );
    expect(textSegs).toHaveLength(1);
    expect(textSegs[0]!.content).toBe("Response");
  });
});

describe("parseContent - inline code spans", () => {
  it("does not match [Thinking] inside backtick code span", () => {
    const text =
      "handling of `[Thinking]`, `[Tool call]`, `[Tool result]`.";
    const segments = parseContent(text, true);
    expect(segments.every((s) => s.type === "text")).toBe(true);
    expect(segments[0]!.content).toContain("`[Thinking]`");
  });

  it("does not match [Bash] inside backtick code span", () => {
    const text = "Run `[Bash]` to execute commands.";
    const segments = parseContent(text, true);
    expect(segments.every((s) => s.type === "text")).toBe(true);
    expect(segments[0]!.content).toContain("`[Bash]`");
  });

  it("still matches real markers outside code spans", () => {
    const text =
      "Mentioned `[Thinking]` in docs.\n[Bash]\n$ echo hi";
    const segments = parseContent(text, true);
    const types = segments.map((s) => s.type);
    expect(types).toContain("text");
    expect(types).toContain("tool");
    expect(types).not.toContain("thinking");
  });

  it("handles double-backtick code spans", () => {
    const text = "Use ``[Thinking]`` as a marker.";
    const segments = parseContent(text, true);
    expect(segments.every((s) => s.type === "text")).toBe(true);
  });

  it("handles double-backtick span containing single backtick", () => {
    const text = "Example: `` `[Thinking]` `` in docs.";
    const segments = parseContent(text, true);
    expect(segments.every((s) => s.type === "text")).toBe(true);
  });

  it("handles triple-backtick inline span", () => {
    const text = "Use ``` [Bash] ``` as inline code.";
    const segments = parseContent(text, true);
    expect(segments.every((s) => s.type === "text")).toBe(true);
  });

  it("handles triple-backtick inline span at line start", () => {
    const text = "``` [Bash] ```\nnext line";
    const segments = parseContent(text, true);
    expect(segments.every((s) => s.type === "text")).toBe(true);
  });

  it("does not confuse fenced code blocks with inline spans", () => {
    const text =
      "```sh\necho [Bash]\n```\n\n[Bash]\n$ echo real";
    const segments = parseContent(text, true);
    const types = segments.map((s) => s.type);
    expect(types).toContain("code");
    expect(types).toContain("tool");
  });

  it("continues scanning after unmatched backtick run", () => {
    const text =
      "Some `` unmatched double then `[Thinking]` single";
    const segments = parseContent(text, true);
    expect(segments.every((s) => s.type === "text")).toBe(true);
    expect(segments[0]!.content).toContain("`[Thinking]`");
  });
});

describe("isToolOnly", () => {
  it("returns false for user messages", () => {
    const msg = makeMsg({ role: "user", content: "[Bash]\nhi" });
    expect(isToolOnly(msg)).toBe(false);
  });

  it("returns false for assistant without tool use", () => {
    const msg = makeMsg({ content: "just text" });
    expect(isToolOnly(msg)).toBe(false);
  });

  it("returns true when content is only tool blocks", () => {
    const msg = makeMsg({
      has_tool_use: true,
      content: "[Bash]\necho hi",
    });
    expect(isToolOnly(msg)).toBe(true);
  });

  it("returns true for multiple tool blocks", () => {
    const msg = makeMsg({
      has_tool_use: true,
      content: "[Read]\nfile.ts\n\n[Edit]\nchanges",
    });
    expect(isToolOnly(msg)).toBe(true);
  });

  it("returns false for plain text assistant messages", () => {
    const msg = makeMsg({ content: "Hello, how can I help?" });
    expect(isToolOnly(msg)).toBe(false);
  });

  it("returns false when text remains after stripping", () => {
    const msg = makeMsg({
      has_tool_use: true,
      content: "Some explanation\n[Bash]\necho hi",
    });
    expect(isToolOnly(msg)).toBe(false);
  });

  it("ignores thinking blocks when checking tool-only", () => {
    const msg = makeMsg({
      has_tool_use: true,
      content: "[Thinking]\nhmm\n[Bash]\necho hi",
    });
    expect(isToolOnly(msg)).toBe(true);
  });

  it("treats codex markers as tool-only content", () => {
    const msg = makeMsg({
      has_tool_use: true,
      content: "[exec_command]\n$ pwd",
    });
    expect(isToolOnly(msg)).toBe(true);
  });
});

describe("parseContent - hasToolUse flag", () => {
  it("skips tool blocks when hasToolUse is false", () => {
    const text = "Some text mentioning [Read: main.go] in prose";
    const segments = parseContent(text, false);
    expect(segments.every(s => s.type !== "tool")).toBe(true);
    expect(segments[0]!.content).toContain("[Read: main.go]");
  });

  it("still parses thinking blocks when hasToolUse is false", () => {
    const text = "[Thinking]\nsome thoughts\n\n[Read: main.go] in text";
    const segments = parseContent(text, false);
    expect(segments[0]!.type).toBe("thinking");
    // The [Read: ...] should be plain text, not a tool block
    const textSegs = segments.filter(s => s.type === "text");
    expect(textSegs.some(s => s.content.includes("[Read: main.go]"))).toBe(true);
  });

  it("still parses code blocks when hasToolUse is false", () => {
    const text = "```js\nconst x = 1\n```\n\n[Bash]\necho hi";
    const segments = parseContent(text, false);
    expect(segments[0]!.type).toBe("code");
    // [Bash] should be plain text
    const textSegs = segments.filter(s => s.type === "text");
    expect(textSegs.some(s => s.content.includes("[Bash]"))).toBe(true);
  });

  it("parses tool blocks normally when hasToolUse is true (default)", () => {
    const segments = parseContent("[Bash]\n$ echo hi");
    expect(segments[0]!.type).toBe("tool");
  });

  it("does not cross-contaminate cache between modes", () => {
    const text = "[Read: file.txt]\nsome output";
    // Parse with tools first — should produce a tool segment
    const withTools = parseContent(text, true);
    expect(withTools[0]!.type).toBe("tool");
    // Parse same text without tools — should be plain text
    const noTools = parseContent(text, false);
    expect(noTools.every((s) => s.type !== "tool")).toBe(true);
    // Reverse order: parse without tools first
    const text2 = "[Edit: other.go]\nreplacement";
    const noTools2 = parseContent(text2, false);
    expect(noTools2.every((s) => s.type !== "tool")).toBe(true);
    const withTools2 = parseContent(text2, true);
    expect(withTools2[0]!.type).toBe("tool");
  });
});

describe("parseContent - Skill tool", () => {
  it("recognizes Skill as a tool block", () => {
    const segments = parseContent(
      "[Skill: superpowers:brainstorming]\nprompt text",
    );
    expect(segments[0]).toEqual({
      type: "tool",
      content: "prompt text",
      label: "Skill : superpowers:brainstorming",
    });
  });

  it("treats Skill-only content as tool-only", () => {
    const msg = makeMsg({
      has_tool_use: true,
      content: "[Skill: commit]\ndo the thing",
    });
    expect(isToolOnly(msg)).toBe(true);
  });
});

describe("parseContent - TaskCreate/TaskUpdate/SendMessage tools", () => {
  it("recognizes TaskCreate as a tool block", () => {
    const segments = parseContent("[TaskCreate: Fix bug]");
    expect(segments[0]!.type).toBe("tool");
    expect(segments[0]!.label).toBe("TaskCreate : Fix bug");
  });

  it("recognizes TaskUpdate as a tool block", () => {
    const segments = parseContent("[TaskUpdate: #5 completed]");
    expect(segments[0]!.type).toBe("tool");
  });

  it("recognizes TaskGet as a tool block", () => {
    const segments = parseContent("[TaskGet: #3]");
    expect(segments[0]!.type).toBe("tool");
    expect(segments[0]!.label).toBe("TaskGet : #3");
  });

  it("recognizes TaskList as a tool block", () => {
    const segments = parseContent("[TaskList]");
    expect(segments[0]!.type).toBe("tool");
    expect(segments[0]!.label).toBe("TaskList");
  });

  it("recognizes SendMessage as a tool block", () => {
    const segments = parseContent("[SendMessage: message to researcher]");
    expect(segments[0]!.type).toBe("tool");
    expect(segments[0]!.label).toBe("SendMessage : message to researcher");
  });
});

describe("enrichSegments", () => {
  it("returns segments unchanged when no tool_calls", () => {
    const segments = parseContent("[Bash]\n$ echo hi");
    const result = enrichSegments(segments);
    expect(result).toBe(segments);
  });

  it("returns segments unchanged for empty tool_calls", () => {
    const segments = parseContent("[Bash]\n$ echo hi");
    const result = enrichSegments(segments, []);
    expect(result).toBe(segments);
  });

  it("attaches toolCall to matching tool segment", () => {
    const segments = parseContent("[Bash]\n$ echo hi");
    const tc: ToolCall = {
      tool_name: "Bash",
      category: "Bash",
      input_json: '{"command":"echo hi","description":""}',
    };
    const result = enrichSegments(segments, [tc]);
    expect(result[0]!.toolCall).toBe(tc);
  });

  it("replaces truncated Bash content with full command", () => {
    // Simulate the \n\n truncation: regex stops at blank line
    const content =
      '[Bash: Create commit]\n$ git commit -m "$(cat <<\'EOF\')\n   Commit message here.';
    const orphaned =
      '\n\n   Co-Authored-By: Claude <noreply@anthropic.com>\n   EOF\n   )"';
    const segments = parseContent(content + orphaned);

    const fullCommand =
      'git commit -m "$(cat <<\'EOF\')\n   Commit message here.\n\n   Co-Authored-By: Claude <noreply@anthropic.com>\n   EOF\n   )"';
    const tc: ToolCall = {
      tool_name: "Bash",
      category: "Bash",
      input_json: JSON.stringify({
        command: fullCommand,
        description: "Create commit",
      }),
    };

    const result = enrichSegments(segments, [tc]);
    // Should have the full command as content
    expect(result[0]!.content).toBe(`$ ${fullCommand}`);
    // Orphaned text should be absorbed
    const textSegments = result.filter((s) => s.type === "text");
    for (const ts of textSegments) {
      // No orphaned fragment from the command should remain
      expect(ts.content).not.toContain("Co-Authored-By");
    }
  });

  it("replaces truncated Codex Bash content with full cmd", () => {
    const firstLine = "cat > file.toml <<'EOF'";
    const fullCmd = `${firstLine}\n[package]\nname = "foo"\nEOF`;
    // Backend truncates to first line; regex sees [Bash]\n$ first-line
    const segments = parseContent(`[exec_command]\n$ ${firstLine}`);

    const tc: ToolCall = {
      tool_name: "exec_command",
      category: "Bash",
      input_json: JSON.stringify({ cmd: fullCmd }),
    };
    const result = enrichSegments(segments, [tc]);
    expect(result[0]!.content).toBe(`$ ${fullCmd}`);
    expect(result[0]!.toolCall).toBe(tc);
  });

  it("does not replace single-line Bash content", () => {
    const segments = parseContent("[Bash]\n$ echo hi");
    const tc: ToolCall = {
      tool_name: "Bash",
      category: "Bash",
      input_json: '{"command":"echo hi"}',
    };
    const result = enrichSegments(segments, [tc]);
    expect(result[0]!.content).toBe("$ echo hi");
  });

  it("attaches toolCall to Task segment", () => {
    const segments = parseContent("[Task: run tests (type)]\n");
    const tc: ToolCall = {
      tool_name: "Task",
      category: "Task",
      input_json: JSON.stringify({
        prompt: "Run all unit tests and report failures",
        subagent_type: "test-runner",
        description: "run tests",
      }),
    };
    const result = enrichSegments(segments, [tc]);
    expect(result[0]!.toolCall).toBe(tc);
  });

  it("matches multiple tool calls in order", () => {
    const segments = parseContent(
      "[Read /foo.ts]\ncontents\n[Edit /foo.ts]\nchanges",
    );
    const tc1: ToolCall = {
      tool_name: "Read",
      category: "Read",
      input_json: '{"file_path":"/foo.ts"}',
    };
    const tc2: ToolCall = {
      tool_name: "Edit",
      category: "Edit",
      input_json: '{"file_path":"/foo.ts"}',
    };
    const result = enrichSegments(segments, [tc1, tc2]);
    expect(result[0]!.toolCall).toBe(tc1);
    expect(result[1]!.toolCall).toBe(tc2);
  });

  it("skips non-tool segments when matching", () => {
    const segments = parseContent(
      "Some text\n[Bash]\n$ echo hi",
    );
    const tc: ToolCall = {
      tool_name: "Bash",
      category: "Bash",
      input_json: '{"command":"echo hi"}',
    };
    const result = enrichSegments(segments, [tc]);
    // Text segment stays unchanged
    expect(result[0]!.type).toBe("text");
    expect(result[0]!.toolCall).toBeUndefined();
    // Tool segment gets the toolCall
    expect(result[1]!.toolCall).toBe(tc);
  });

  it("appends remaining tool_calls when more exist than tool segments", () => {
    const segments = parseContent("[Bash]\n$ echo hi");
    const tc1: ToolCall = {
      tool_name: "Bash",
      category: "Bash",
      input_json: '{"command":"echo hi"}',
    };
    const tc2: ToolCall = {
      tool_name: "Read",
      category: "Read",
    };
    const result = enrichSegments(segments, [tc1, tc2]);
    expect(result[0]!.toolCall).toBe(tc1);
    expect(result).toHaveLength(2);
    expect(result[1]!.toolCall).toBe(tc2);
    expect(result[1]!.type).toBe("tool");
  });

  it("creates tool segments from structured tool_calls when no text markers exist (pi/omp style)", () => {
    // Pi/omp sessions: content is plain text, tool calls are structured JSON
    const segments = parseContent("I'll read the file.");
    const tc1: ToolCall = { tool_name: "read", category: "Read", input_json: '{"path":"/foo.ts"}' };
    const tc2: ToolCall = { tool_name: "write", category: "Write", input_json: '{"path":"/bar.ts","content":"x"}' };
    const result = enrichSegments(segments, [tc1, tc2]);
    const toolSegs = result.filter((s) => s.type === "tool");
    expect(toolSegs).toHaveLength(2);
    expect(toolSegs[0]!.toolCall).toBe(tc1);
    expect(toolSegs[0]!.label).toBe("Read");
    expect(toolSegs[1]!.toolCall).toBe(tc2);
    expect(toolSegs[1]!.label).toBe("Write");
  });

  it("creates tool segments from structured tool_calls when content is empty (pi/omp tool-only message)", () => {
    const segments = parseContent("");
    const tc: ToolCall = { tool_name: "bash", category: "Bash", input_json: '{"command":"ls"}' };
    const result = enrichSegments(segments, [tc]);
    expect(result).toHaveLength(1);
    expect(result[0]!.type).toBe("tool");
    expect(result[0]!.toolCall).toBe(tc);
    expect(result[0]!.label).toBe("Bash");
  });
});

describe("enrichSegments - pi tool aliasing", () => {
  function makeStructuredSegments(toolCalls: ToolCall[]): ReturnType<typeof enrichSegments> {
    // Pi sessions have plain text content; tool calls come from structured JSON
    const segments = parseContent("I'll work on the file.");
    return enrichSegments(segments, toolCalls);
  }

  it("aliases str_replace to Edit label", () => {
    const tc: ToolCall = {
      tool_name: "str_replace",
      category: "Edit",
      input_json: '{"path":"/src/app.ts","old_string":"x","new_string":"y"}',
    };
    const result = makeStructuredSegments([tc]);
    const toolSegs = result.filter((s) => s.type === "tool");
    expect(toolSegs).toHaveLength(1);
    expect(toolSegs[0]!.label).toBe("Edit");
  });

  it("aliases run_command to Bash label", () => {
    const tc: ToolCall = {
      tool_name: "run_command",
      category: "Bash",
      input_json: '{"command":"npm test"}',
    };
    const result = makeStructuredSegments([tc]);
    const toolSegs = result.filter((s) => s.type === "tool");
    expect(toolSegs).toHaveLength(1);
    expect(toolSegs[0]!.label).toBe("Bash");
  });

  it("aliases create_file to Write label", () => {
    const tc: ToolCall = {
      tool_name: "create_file",
      category: "Write",
      input_json: '{"path":"/src/new.ts","content":"export const x = 1;"}',
    };
    const result = makeStructuredSegments([tc]);
    const toolSegs = result.filter((s) => s.type === "tool");
    expect(toolSegs).toHaveLength(1);
    expect(toolSegs[0]!.label).toBe("Write");
  });

  it("aliases read_file to Read label", () => {
    const tc: ToolCall = {
      tool_name: "read_file",
      category: "Read",
      input_json: '{"path":"/src/app.ts"}',
    };
    const result = makeStructuredSegments([tc]);
    const toolSegs = result.filter((s) => s.type === "tool");
    expect(toolSegs).toHaveLength(1);
    expect(toolSegs[0]!.label).toBe("Read");
  });

  it("expands multi-line run_command to $ command format", () => {
    const segments = parseContent("");
    const tc: ToolCall = {
      tool_name: "run_command",
      category: "Bash",
      input_json: JSON.stringify({ command: "line1\nline2" }),
    };
    const result = enrichSegments(segments, [tc]);
    expect(result).toHaveLength(1);
    expect(result[0]!.content).toBe("$ line1\nline2");
  });

  it("expands single-line run_command to $ command format", () => {
    const segments = parseContent("");
    const tc: ToolCall = {
      tool_name: "run_command",
      category: "Bash",
      input_json: JSON.stringify({ command: "mkdir -p dist" }),
    };
    const result = enrichSegments(segments, [tc]);
    expect(result).toHaveLength(1);
    expect(result[0]!.content).toBe("$ mkdir -p dist");
  });

  it("sets empty content for non-Bash pi tools so ToolBlock uses fallbackContent", () => {
    const tc: ToolCall = {
      tool_name: "str_replace",
      category: "Edit",
      input_json: '{"path":"/src/app.ts","old_str":"x","new_str":"y"}',
    };
    const result = makeStructuredSegments([tc]);
    const toolSegs = result.filter((s) => s.type === "tool");
    expect(toolSegs[0]!.content).toBe("");
  });
});

describe("enrichSegments - pi lowercase native tool aliases", () => {
  function makeStructuredSegments(toolCalls: ToolCall[]): ReturnType<typeof enrichSegments> {
    const segments = parseContent("Working on files.");
    return enrichSegments(segments, toolCalls);
  }

  it("aliases lowercase bash to Bash label", () => {
    const tc: ToolCall = {
      tool_name: "bash",
      category: "Bash",
      input_json: '{"command":"ls -la","agent__intent":"List files"}',
    };
    const result = makeStructuredSegments([tc]);
    const toolSegs = result.filter((s) => s.type === "tool");
    expect(toolSegs).toHaveLength(1);
    expect(toolSegs[0]!.label).toBe("Bash");
  });

  it("aliases lowercase read to Read label", () => {
    const tc: ToolCall = {
      tool_name: "read",
      category: "Read",
      input_json: '{"path":"/src/app.ts"}',
    };
    const result = makeStructuredSegments([tc]);
    const toolSegs = result.filter((s) => s.type === "tool");
    expect(toolSegs).toHaveLength(1);
    expect(toolSegs[0]!.label).toBe("Read");
  });

  it("aliases lowercase write to Write label", () => {
    const tc: ToolCall = {
      tool_name: "write",
      category: "Write",
      input_json: '{"path":"/src/new.ts","content":"x"}',
    };
    const result = makeStructuredSegments([tc]);
    const toolSegs = result.filter((s) => s.type === "tool");
    expect(toolSegs).toHaveLength(1);
    expect(toolSegs[0]!.label).toBe("Write");
  });

  it("aliases lowercase edit to Edit label", () => {
    const tc: ToolCall = {
      tool_name: "edit",
      category: "Edit",
      input_json: '{"path":"/src/app.ts","edits":[]}',
    };
    const result = makeStructuredSegments([tc]);
    const toolSegs = result.filter((s) => s.type === "tool");
    expect(toolSegs).toHaveLength(1);
    expect(toolSegs[0]!.label).toBe("Edit");
  });

  it("aliases lowercase grep to Grep label", () => {
    const tc: ToolCall = {
      tool_name: "grep",
      category: "Grep",
      input_json: '{"pattern":"TODO","path":"/src"}',
    };
    const result = makeStructuredSegments([tc]);
    const toolSegs = result.filter((s) => s.type === "tool");
    expect(toolSegs).toHaveLength(1);
    expect(toolSegs[0]!.label).toBe("Grep");
  });

  it("aliases lowercase glob to Glob label", () => {
    const tc: ToolCall = {
      tool_name: "glob",
      category: "Glob",
      input_json: '{"pattern":"**/*.ts"}',
    };
    const result = makeStructuredSegments([tc]);
    const toolSegs = result.filter((s) => s.type === "tool");
    expect(toolSegs).toHaveLength(1);
    expect(toolSegs[0]!.label).toBe("Glob");
  });

  it("aliases find to Read label and extracts pattern preview", () => {
    const tc: ToolCall = {
      tool_name: "find",
      category: "Read",
      input_json: '{"pattern":"*.go"}',
    };
    const result = makeStructuredSegments([tc]);
    const toolSegs = result.filter((s) => s.type === "tool");
    expect(toolSegs).toHaveLength(1);
    expect(toolSegs[0]!.label).toBe("Read");
    expect(toolSegs[0]!.content).toBe("*.go");
  });

  it("expands lowercase bash command to $ format", () => {
    const segments = parseContent("");
    const tc: ToolCall = {
      tool_name: "bash",
      category: "Bash",
      input_json: '{"command":"npm test","agent__intent":"Run tests"}',
    };
    const result = enrichSegments(segments, [tc]);
    expect(result[0]!.content).toBe("$ npm test");
  });

  it("expands multi-line lowercase bash to $ format", () => {
    const segments = parseContent("");
    const tc: ToolCall = {
      tool_name: "bash",
      category: "Bash",
      input_json: JSON.stringify({ command: "git commit -m \"$(cat <<'EOF')\nMessage\nEOF\n)\"" }),
    };
    const result = enrichSegments(segments, [tc]);
    expect(result[0]!.content).toContain("$ git commit");
  });
});

describe("enrichSegments - Read path preview", () => {
  it("sets content to file path for lowercase read tool", () => {
    const segments = parseContent("");
    const tc: ToolCall = {
      tool_name: "read",
      category: "Read",
      input_json: '{"path":"/src/auth.go","agent__intent":"Reading auth module"}',
    };
    const result = enrichSegments(segments, [tc]);
    expect(result[0]!.content).toBe("/src/auth.go");
  });

  it("sets content to file path for read_file tool", () => {
    const segments = parseContent("");
    const tc: ToolCall = {
      tool_name: "read_file",
      category: "Read",
      input_json: '{"path":"/src/main.go"}',
    };
    const result = enrichSegments(segments, [tc]);
    expect(result[0]!.content).toBe("/src/main.go");
  });

  it("prefers path over file_path for read tool (pi field name)", () => {
    const segments = parseContent("");
    const tc: ToolCall = {
      tool_name: "read",
      category: "Read",
      input_json: '{"path":"/src/app.ts","file_path":"/src/other.ts"}',
    };
    const result = enrichSegments(segments, [tc]);
    expect(result[0]!.content).toBe("/src/app.ts");
  });

  it("leaves content empty for read tool with no path", () => {
    const segments = parseContent("");
    const tc: ToolCall = {
      tool_name: "read",
      category: "Read",
      input_json: '{"agent__intent":"reading something"}',
    };
    const result = enrichSegments(segments, [tc]);
    expect(result[0]!.content).toBe("");
  });
});

describe("parseContent + enrichSegments segment typing pipeline", () => {
  it("correctly types thinking segments", () => {
    const segs = parseContent("[Thinking]\ndeep thoughts\n[/Thinking]", false);
    expect(segs).toHaveLength(1);
    expect(segs[0]!.type).toBe("thinking");
  });

  it("correctly types tool segments", () => {
    const segs = parseContent("[Bash]\n$ echo hi", true);
    expect(segs).toHaveLength(1);
    expect(segs[0]!.type).toBe("tool");
  });

  it("correctly types code segments", () => {
    const segs = parseContent("```js\nconst x = 1;\n```", false);
    expect(segs).toHaveLength(1);
    expect(segs[0]!.type).toBe("code");
  });

  it("correctly types plain text segments", () => {
    const segs = parseContent("Hello world", false);
    expect(segs).toHaveLength(1);
    expect(segs[0]!.type).toBe("text");
  });

  it("produces all four segment types from mixed content", () => {
    const text =
      "[Thinking]\nhmm\n[/Thinking]\n\n" +
      "Here is my analysis.\n\n" +
      "```ts\nconst x = 1;\n```\n\n" +
      "[Bash]\n$ echo done";
    const segs = parseContent(text, true);
    const types = segs.map((s) => s.type);
    expect(types).toContain("thinking");
    expect(types).toContain("text");
    expect(types).toContain("code");
    expect(types).toContain("tool");
  });

  it("enrichSegments preserves segment types through enrichment", () => {
    const segs = parseContent("Some text\n[Bash]\n$ echo hi", true);
    const tc: ToolCall = {
      tool_name: "Bash",
      category: "Bash",
      input_json: '{"command":"echo hi"}',
    };
    const enriched = enrichSegments(segs, [tc]);
    expect(enriched[0]!.type).toBe("text");
    expect(enriched[1]!.type).toBe("tool");
    expect(enriched[1]!.toolCall).toBe(tc);
  });
});

describe("hasVisibleSegments", () => {
  /** Helper: create a visibility predicate from a set of visible types */
  function visibilityFrom(
    visible: Set<string>,
  ): (type: string) => boolean {
    return (type: string) => visible.has(type);
  }

  const allBlocksVisible = visibilityFrom(
    new Set(["user", "assistant", "thinking", "tool", "code"]),
  );

  it("tool-only message hidden when tool filter is off", () => {
    const m = makeMsg({
      content: "[Bash]\n$ echo hi",
      has_tool_use: true,
    });
    const noTool = visibilityFrom(
      new Set(["user", "assistant", "thinking", "code"]),
    );
    expect(hasVisibleSegments(m, noTool)).toBe(false);
  });

  it("tool-only message visible when tool filter is on", () => {
    const m = makeMsg({
      content: "[Bash]\n$ echo hi",
      has_tool_use: true,
    });
    expect(hasVisibleSegments(m, allBlocksVisible)).toBe(true);
  });

  it("assistant text message hidden when assistant filter is off", () => {
    const m = makeMsg({
      content: "Here is my response.",
    });
    const noAssistant = visibilityFrom(
      new Set(["user", "thinking", "tool", "code"]),
    );
    expect(hasVisibleSegments(m, noAssistant)).toBe(false);
  });

  it("assistant text hidden but code/tool segments still visible", () => {
    const m = makeMsg({
      content: "Let me explain.\n\n[Bash]\n$ ls",
      has_tool_use: true,
    });
    const noAssistant = visibilityFrom(
      new Set(["user", "thinking", "tool", "code"]),
    );
    // The tool segment should keep the message visible
    expect(hasVisibleSegments(m, noAssistant)).toBe(true);
  });

  it("thinking-only message hidden when thinking filter is off", () => {
    const m = makeMsg({
      content: "[Thinking]\ndeep thoughts\n[/Thinking]",
      has_thinking: true,
    });
    const noThinking = visibilityFrom(
      new Set(["user", "assistant", "tool", "code"]),
    );
    expect(hasVisibleSegments(m, noThinking)).toBe(false);
  });

  it("thinking-only message visible when thinking filter is on", () => {
    const m = makeMsg({
      content: "[Thinking]\ndeep thoughts\n[/Thinking]",
      has_thinking: true,
    });
    expect(hasVisibleSegments(m, allBlocksVisible)).toBe(true);
  });

  it("message with mixed segments (text + tool) partially visible when only tool hidden", () => {
    const m = makeMsg({
      content: "Let me check.\n\n[Bash]\n$ ls",
      has_tool_use: true,
    });
    const noTool = visibilityFrom(
      new Set(["user", "assistant", "thinking", "code"]),
    );
    // Text segment maps to "assistant" which is visible
    expect(hasVisibleSegments(m, noTool)).toBe(true);
  });

  it("message with mixed segments (text + tool) partially visible when only assistant hidden", () => {
    const m = makeMsg({
      content: "Let me check.\n\n[Bash]\n$ ls",
      has_tool_use: true,
    });
    const noAssistant = visibilityFrom(
      new Set(["user", "thinking", "tool", "code"]),
    );
    // Tool segment is still visible
    expect(hasVisibleSegments(m, noAssistant)).toBe(true);
  });

  it("user message hidden when user filter is off", () => {
    const m = makeMsg({
      role: "user",
      content: "Please help me with this.",
    });
    const noUser = visibilityFrom(
      new Set(["assistant", "thinking", "tool", "code"]),
    );
    expect(hasVisibleSegments(m, noUser)).toBe(false);
  });

  it("user message visible when user filter is on", () => {
    const m = makeMsg({
      role: "user",
      content: "Please help me with this.",
    });
    expect(hasVisibleSegments(m, allBlocksVisible)).toBe(true);
  });

  it("empty assistant message stays visible when role filter allows", () => {
    const m = makeMsg({
      content: "",
    });
    expect(hasVisibleSegments(m, allBlocksVisible)).toBe(true);
  });

  it("empty assistant message hidden when assistant filter is off", () => {
    const m = makeMsg({
      content: "",
    });
    const noAssistant = visibilityFrom(
      new Set(["user", "thinking", "tool", "code"]),
    );
    expect(hasVisibleSegments(m, noAssistant)).toBe(false);
  });

  it("empty user message stays visible when user role filter allows", () => {
    const m = makeMsg({
      role: "user",
      content: "",
    });
    const onlyUser = visibilityFrom(new Set(["user"]));
    expect(hasVisibleSegments(m, onlyUser)).toBe(true);
  });

  it("message with thinking + tool visible when either is visible", () => {
    const m = makeMsg({
      content: "[Thinking]\nhmm\n[Bash]\n$ echo hi",
      has_tool_use: true,
      has_thinking: true,
    });
    const onlyThinking = visibilityFrom(new Set(["thinking"]));
    expect(hasVisibleSegments(m, onlyThinking)).toBe(true);
    const onlyTool = visibilityFrom(new Set(["tool"]));
    expect(hasVisibleSegments(m, onlyTool)).toBe(true);
  });

  it("message with thinking + tool hidden when neither is visible", () => {
    const m = makeMsg({
      content: "[Thinking]\nhmm\n[Bash]\n$ echo hi",
      has_tool_use: true,
      has_thinking: true,
    });
    const noThinkingNoTool = visibilityFrom(
      new Set(["user", "assistant", "code"]),
    );
    expect(hasVisibleSegments(m, noThinkingNoTool)).toBe(false);
  });

  it("code block message visible when code filter is on", () => {
    const m = makeMsg({
      content: "```js\nconst x = 1;\n```",
    });
    const onlyCode = visibilityFrom(new Set(["code"]));
    expect(hasVisibleSegments(m, onlyCode)).toBe(true);
  });

  it("code block message hidden when code filter is off", () => {
    const m = makeMsg({
      content: "```js\nconst x = 1;\n```",
    });
    const noCode = visibilityFrom(
      new Set(["user", "assistant", "thinking", "tool"]),
    );
    expect(hasVisibleSegments(m, noCode)).toBe(false);
  });

  it("message with text + code visible when only code is visible", () => {
    const m = makeMsg({
      content: "Here is the code:\n\n```ts\nconst x = 1;\n```",
    });
    const onlyCode = visibilityFrom(new Set(["code"]));
    expect(hasVisibleSegments(m, onlyCode)).toBe(true);
  });

  it("everything hidden returns false for all segment types", () => {
    const m = makeMsg({
      content: "Hello\n[Thinking]\nhmm\n[Bash]\n$ ls",
      has_tool_use: true,
      has_thinking: true,
    });
    const nothingVisible = visibilityFrom(new Set<string>());
    expect(hasVisibleSegments(m, nothingVisible)).toBe(false);
  });

  it("skill segment hidden when parent role filter is off", () => {
    const m = makeMsg({
      content: "[Skill: commit]\nRunning commit skill\n[/Skill]",
    });
    const noAssistant = visibilityFrom(
      new Set(["user", "thinking", "tool", "code"]),
    );
    expect(hasVisibleSegments(m, noAssistant)).toBe(false);
  });

  it("skill segment visible when parent role filter is on", () => {
    const m = makeMsg({
      content: "[Skill: commit]\nRunning commit skill\n[/Skill]",
    });
    expect(hasVisibleSegments(m, allBlocksVisible)).toBe(true);
  });

  it("user skill segment hidden when user filter is off", () => {
    const m = makeMsg({
      role: "user",
      content: "[Skill: commit]\nRunning commit skill\n[/Skill]",
    });
    const noUser = visibilityFrom(
      new Set(["assistant", "thinking", "tool", "code"]),
    );
    expect(hasVisibleSegments(m, noUser)).toBe(false);
  });

  it("assistant skill visible when role is on but tool filter is off", () => {
    const m = makeMsg({
      content: "[Skill: commit]\nRunning commit skill\n[/Skill]",
    });
    const noTool = visibilityFrom(
      new Set(["user", "assistant", "thinking", "code"]),
    );
    expect(hasVisibleSegments(m, noTool)).toBe(true);
  });

  it("user skill visible when role is on but tool filter is off", () => {
    const m = makeMsg({
      role: "user",
      content: "[Skill: commit]\nRunning commit skill\n[/Skill]",
    });
    const noTool = visibilityFrom(
      new Set(["user", "assistant", "thinking", "code"]),
    );
    expect(hasVisibleSegments(m, noTool)).toBe(true);
  });
});
