import { describe, expect, it } from "vitest";
import {
  truncate,
  extractToolParamMeta,
  generateFallbackContent,
} from "./tool-params.js";

describe("truncate", () => {
  it("returns short strings unchanged", () => {
    expect(truncate("hello", 10)).toBe("hello");
  });

  it("truncates at max and appends ellipsis", () => {
    expect(truncate("abcdef", 3)).toBe("abc\u2026");
  });

  it("returns exact-length strings unchanged", () => {
    expect(truncate("abc", 3)).toBe("abc");
  });
});

describe("extractToolParamMeta", () => {
  it("returns null for Task tool", () => {
    expect(
      extractToolParamMeta("Task", { prompt: "do stuff" }),
    ).toBeNull();
  });

  it("returns null for TaskCreate tool", () => {
    expect(
      extractToolParamMeta("TaskCreate", { subject: "x" }),
    ).toBeNull();
  });

  it("returns null for TaskUpdate tool", () => {
    expect(
      extractToolParamMeta("TaskUpdate", { taskId: "1" }),
    ).toBeNull();
  });

  it("extracts Read params", () => {
    const meta = extractToolParamMeta("Read", {
      file_path: "/src/app.ts",
      offset: 10,
      limit: 50,
    });
    expect(meta).toEqual([
      { label: "file", value: "/src/app.ts" },
      { label: "offset", value: "10" },
      { label: "limit", value: "50" },
    ]);
  });

  it("extracts Read pages param", () => {
    const meta = extractToolParamMeta("Read", {
      file_path: "/doc.pdf",
      pages: "1-5",
    });
    expect(meta).toEqual([
      { label: "file", value: "/doc.pdf" },
      { label: "pages", value: "1-5" },
    ]);
  });

  it("extracts Edit params with replace_all", () => {
    const meta = extractToolParamMeta("Edit", {
      file_path: "/src/app.ts",
      replace_all: true,
    });
    expect(meta).toEqual([
      { label: "file", value: "/src/app.ts" },
      { label: "mode", value: "replace_all" },
    ]);
  });

  it("extracts Write file_path", () => {
    const meta = extractToolParamMeta("Write", {
      file_path: "/src/new.ts",
      content: "export const x = 1;",
    });
    expect(meta).toEqual([
      { label: "file", value: "/src/new.ts" },
    ]);
  });

  it("extracts Grep params", () => {
    const meta = extractToolParamMeta("Grep", {
      pattern: "TODO",
      path: "/src",
      glob: "*.ts",
      output_mode: "content",
    });
    expect(meta).toEqual([
      { label: "pattern", value: "TODO" },
      { label: "path", value: "/src" },
      { label: "glob", value: "*.ts" },
      { label: "mode", value: "content" },
    ]);
  });

  it("extracts Glob params", () => {
    const meta = extractToolParamMeta("Glob", {
      pattern: "**/*.ts",
      path: "/src",
    });
    expect(meta).toEqual([
      { label: "pattern", value: "**/*.ts" },
      { label: "path", value: "/src" },
    ]);
  });

  it("extracts Bash description", () => {
    const meta = extractToolParamMeta("Bash", {
      command: "npm test",
      description: "Run test suite",
    });
    expect(meta).toEqual([
      { label: "description", value: "Run test suite" },
    ]);
  });

  it("returns null for Bash without description", () => {
    expect(
      extractToolParamMeta("Bash", { command: "ls" }),
    ).toBeNull();
  });

  it("extracts Skill name", () => {
    const meta = extractToolParamMeta("Skill", {
      skill: "commit",
    });
    expect(meta).toEqual([
      { label: "skill", value: "commit" },
    ]);
  });

  it("returns null for unknown tool with no matching params", () => {
    expect(
      extractToolParamMeta("CustomTool", { foo: "bar" }),
    ).toBeNull();
  });

  it("preserves zero-valued offset and limit", () => {
    const meta = extractToolParamMeta("Read", {
      file_path: "/src/app.ts",
      offset: 0,
      limit: 0,
    });
    expect(meta).toEqual([
      { label: "file", value: "/src/app.ts" },
      { label: "offset", value: "0" },
      { label: "limit", value: "0" },
    ]);
  });

  it("truncates long file paths", () => {
    const longPath = "/a".repeat(50);
    const meta = extractToolParamMeta("Read", {
      file_path: longPath,
    });
    expect(meta![0]!.value.length).toBeLessThanOrEqual(81);
    expect(meta![0]!.value).toContain("\u2026");
  });

  it("extracts Read file path from pi 'path' field", () => {
    const meta = extractToolParamMeta("Read", { path: "/src/app.ts" });
    expect(meta).toEqual([{ label: "file", value: "/src/app.ts" }]);
  });

  it("prefers file_path over path for Read", () => {
    const meta = extractToolParamMeta("Read", {
      file_path: "/a.ts",
      path: "/b.ts",
    });
    expect(meta![0]!.value).toBe("/a.ts");
  });

  it("extracts Edit file path from pi 'path' field", () => {
    const meta = extractToolParamMeta("Edit", { path: "/src/app.ts" });
    expect(meta).toEqual([{ label: "file", value: "/src/app.ts" }]);
  });

  it("extracts Edit file path from opencode 'filePath' field", () => {
    const meta = extractToolParamMeta("Edit", {
      filePath: "/src/app.ts",
      oldString: "x",
      newString: "y",
    });
    expect(meta).toEqual([{ label: "file", value: "/src/app.ts" }]);
  });

  it("extracts Write file path from pi 'path' field", () => {
    const meta = extractToolParamMeta("Write", { path: "/src/new.ts" });
    expect(meta).toEqual([{ label: "file", value: "/src/new.ts" }]);
  });
});

describe("generateFallbackContent", () => {
  it("returns null for Task tool", () => {
    expect(
      generateFallbackContent("Task", { prompt: "do stuff" }),
    ).toBeNull();
  });

  it("shows diff for Edit tool", () => {
    const result = generateFallbackContent("Edit", {
      file_path: "/src/app.ts",
      old_string: "const x = 1;",
      new_string: "const x = 2;",
    });
    expect(result).toBe(
      "--- old\nconst x = 1;\n+++ new\nconst x = 2;",
    );
  });

  it("shows diff for Edit tool using pi old_str/new_str field names", () => {
    const result = generateFallbackContent("Edit", {
      path: "/src/app.ts",
      old_str: "const x = 1;",
      new_str: "const x = 2;",
    });
    expect(result).toBe(
      "--- old\nconst x = 1;\n+++ new\nconst x = 2;",
    );
  });

  it("shows only new_string when old_string is empty", () => {
    const result = generateFallbackContent("Edit", {
      file_path: "/src/app.ts",
      old_string: "",
      new_string: "const x = 1;",
    });
    expect(result).toBe(
      "--- old\n\n+++ new\nconst x = 1;",
    );
  });

  it("shows diff for Edit tool using opencode camelCase field names", () => {
    const result = generateFallbackContent("Edit", {
      filePath: "/src/styles.css",
      oldString: ".foo { color: red; }",
      newString: ".foo { color: blue; }",
    });
    expect(result).toBe(
      "--- old\n.foo { color: red; }\n+++ new\n.foo { color: blue; }",
    );
  });

  it("shows pi edits array with set_line", () => {
    const result = generateFallbackContent("Edit", {
      path: "src/styles.css",
      edits: [
        {
          set_line: {
            anchor: "12:0",
            new_text: ".foo { color: blue; }",
          },
        },
      ],
    });
    expect(result).toBe("@ 12:0\n.foo { color: blue; }");
  });

  it("shows pi edits array with replace_lines", () => {
    const result = generateFallbackContent("Edit", {
      path: "src/data/site.ts",
      edits: [
        {
          replace_lines: {
            start_anchor: "31:b4",
            end_anchor: "37:f6",
            new_text: "const updated = true;",
          },
        },
      ],
    });
    expect(result).toBe("@ 31:b4..37:f6\nconst updated = true;");
  });

  it("shows pi edits replace_lines deletion (empty new_text)", () => {
    const result = generateFallbackContent("Edit", {
      path: "src/data/site.ts",
      edits: [
        {
          replace_lines: {
            start_anchor: "31:b4",
            end_anchor: "37:f6",
            new_text: "",
          },
        },
      ],
    });
    expect(result).toBe("@ 31:b4..37:f6\n(delete)");
  });

  it("shows pi edits array with insert_after", () => {
    const result = generateFallbackContent("Edit", {
      path: "src/styles.css",
      edits: [
        {
          insert_after: {
            anchor: "248:18",
            text: ".new-class { color: red; }",
          },
        },
      ],
    });
    expect(result).toBe(
      "insert after 248:18\n.new-class { color: red; }",
    );
  });

  it("shows pi edits insert_after with empty text", () => {
    const result = generateFallbackContent("Edit", {
      path: "src/styles.css",
      edits: [
        {
          insert_after: {
            anchor: "248:18",
            text: "",
          },
        },
      ],
    });
    expect(result).toBe("insert after 248:18\n(empty)");
  });

  it("shows pi edits set_line deletion (empty new_text)", () => {
    const result = generateFallbackContent("Edit", {
      path: "src/pages/index.astro",
      edits: [
        {
          set_line: {
            anchor: "17:0b",
            new_text: "",
          },
        },
      ],
    });
    expect(result).toBe("@ 17:0b\n(delete)");
  });

  it("shows pi edits array with op/tag/content", () => {
    const result = generateFallbackContent("Edit", {
      path: "content.js",
      edits: [
        {
          op: "set",
          tag: "384#BH",
          content: ["line1", "line2"],
        },
      ],
    });
    expect(result).toBe("tag: 384#BH\nline1\nline2");
  });

  it("shows pi edits array with op/pos/lines (real Pi agent format)", () => {
    const result = generateFallbackContent("Edit", {
      path: "src/styles.css",
      edits: [
        {
          op: "replace",
          pos: "846#VH",
          end: "851#WT",
          lines: [".foo {", "  color: blue;", "}"],
        },
      ],
      agent__intent: "Update styles",
    });
    expect(result).toBe("replace @ 846#VH\n.foo {\n  color: blue;\n}");
  });

  it("shows pi edits op/pos/lines without end field", () => {
    const result = generateFallbackContent("Edit", {
      path: "src/app.ts",
      edits: [
        {
          op: "insert",
          pos: "10#AB",
          lines: ["const x = 1;"],
        },
      ],
    });
    expect(result).toBe("insert @ 10#AB\nconst x = 1;");
  });

  it("returns null for Edit with no recognized diff fields", () => {
    expect(
      generateFallbackContent("Edit", { file_path: "/src/app.ts" }),
    ).toBeNull();
  });

  it("truncates long Edit strings", () => {
    const long = "x".repeat(600);
    const result = generateFallbackContent("Edit", {
      old_string: long,
      new_string: "short",
    })!;
    const oldLine = result.split("\n")[1]!;
    expect(oldLine.length).toBeLessThanOrEqual(501);
    expect(oldLine).toContain("\u2026");
  });

  it("shows Write content preview", () => {
    const result = generateFallbackContent("Write", {
      file_path: "/src/new.ts",
      content: 'export const x = "hello";',
    });
    expect(result).toBe('export const x = "hello";');
  });

  it("truncates long Write content", () => {
    const long = "line\n".repeat(200);
    const result = generateFallbackContent("Write", {
      file_path: "/src/big.ts",
      content: long,
    })!;
    expect(result.length).toBeLessThanOrEqual(501);
  });

  it("shows empty-file marker for Write with empty content", () => {
    expect(
      generateFallbackContent("Write", {
        file_path: "/src/empty.ts",
        content: "",
      }),
    ).toBe("(empty file)");
  });

  it("falls back to generic display for Write without content", () => {
    expect(
      generateFallbackContent("Write", {
        file_path: "/src/new.ts",
      }),
    ).toBe("file_path: /src/new.ts");
  });

  it("shows generic key-value for Read", () => {
    const result = generateFallbackContent("Read", {
      file_path: "/src/app.ts",
      limit: 100,
    });
    expect(result).toBe(
      "file_path: /src/app.ts\nlimit: 100",
    );
  });

  it("shows generic key-value for unknown tools", () => {
    const result = generateFallbackContent("CustomTool", {
      foo: "bar",
      count: 42,
    });
    expect(result).toBe("foo: bar\ncount: 42");
  });

  it("skips null and empty values in generic mode", () => {
    const result = generateFallbackContent("CustomTool", {
      present: "yes",
      missing: null,
      empty: "",
    });
    expect(result).toBe("present: yes");
  });

  it("stringifies non-string values in generic mode", () => {
    const result = generateFallbackContent("CustomTool", {
      arr: [1, 2, 3],
      obj: { nested: true },
    });
    expect(result).toBe(
      "arr: [1,2,3]\nobj: {\"nested\":true}",
    );
  });

  it("returns null when params are all empty", () => {
    expect(
      generateFallbackContent("CustomTool", {}),
    ).toBeNull();
  });
});

describe("generateFallbackContent - agent__intent filtering", () => {
  it("does not include agent__intent in generic key-value output", () => {
    const result = generateFallbackContent("CustomTool", {
      command: "ls -la",
      agent__intent: "Listing files in directory",
    });
    expect(result).not.toContain("agent__intent");
    expect(result).toContain("command: ls -la");
  });

  it("does not include agent__intent for bash tool", () => {
    const result = generateFallbackContent("Bash", {
      command: "npm test",
      agent__intent: "Running test suite",
    });
    // Bash has no special handler for command; falls to generic loop
    // agent__intent must not appear; command may appear
    expect(result).not.toContain("agent__intent");
  });

  it("does not include agent__intent for read tool (generic path)", () => {
    const result = generateFallbackContent("Read", {
      path: "/src/app.ts",
      agent__intent: "Reading auth module",
    });
    expect(result).not.toContain("agent__intent");
  });

  it("returns null when only agent__intent is present", () => {
    const result = generateFallbackContent("CustomTool", {
      agent__intent: "Doing something",
    });
    expect(result).toBeNull();
  });

  it("shows other params when agent__intent is mixed in", () => {
    const result = generateFallbackContent("CustomTool", {
      foo: "bar",
      agent__intent: "Something",
      baz: "qux",
    });
    expect(result).toBe("foo: bar\nbaz: qux");
    expect(result).not.toContain("agent__intent");
  });
});
