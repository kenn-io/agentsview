import { describe, expect, it } from "vite-plus/test";
import {
  truncate,
  pathDisplayValue,
  extractToolParamMeta,
  generateFallbackContent,
  toolCopyText,
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

describe("pathDisplayValue", () => {
  it.each([
    ["/" + "a".repeat(90), "/" + "a".repeat(90)],
    ["C:\\" + "a".repeat(85), "C:\\" + "a".repeat(85)],
    ["\\\\server\\share\\" + "a".repeat(75), "\\\\server\\share\\" + "a".repeat(75)],
  ])("preserves root markers for long paths", (path, expected) => {
    expect(pathDisplayValue(path)).toBe(expected);
  });

  it("caps long relative paths", () => {
    const path = "src/" + "nested/".repeat(30) + "file.ts";
    expect(pathDisplayValue(path)).toBe(path.slice(0, 100) + "…");
  });
});

describe("extractToolParamMeta", () => {
  it("returns null for Task tool", () => {
    expect(extractToolParamMeta("Task", { prompt: "do stuff" })).toBeNull();
  });

  it("returns null for TaskCreate tool", () => {
    expect(extractToolParamMeta("TaskCreate", { subject: "x" })).toBeNull();
  });

  it("returns null for TaskUpdate tool", () => {
    expect(extractToolParamMeta("TaskUpdate", { taskId: "1" })).toBeNull();
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
    expect(meta).toEqual([{ label: "file", value: "/src/new.ts" }]);
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

  it("shows an object parameter as JSON", () => {
    const meta = extractToolParamMeta("Grep", { pattern: { regex: "TODO", flags: "i" } });
    expect(meta).toEqual([{ label: "pattern", value: '{"regex":"TODO","flags":"i"}' }]);
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

  it("labels Bash parameters other than the command, which is the body", () => {
    const meta = extractToolParamMeta("Bash", {
      command: "npm test",
      description: "Run test suite",
      cwd: "/repo/frontend",
      timeout: 120000,
      env: { CI: "true" },
      agent__intent: "internal note",
    });
    expect(meta).toEqual([
      { label: "description", value: "Run test suite" },
      { label: "cwd", value: "/repo/frontend" },
      { label: "timeout", value: "120000" },
      { label: "env", value: '{"CI":"true"}' },
    ]);
  });

  it("shortens a long Bash working directory and keeps the full path", () => {
    const workdir =
      "/home/example/projects/a-very-long-directory-name/with-more-nesting/another-long-segment/frontend";
    const meta = extractToolParamMeta("exec_command", { cmd: "ls", workdir }, "Bash");
    expect(meta).toEqual([
      { label: "workdir", value: workdir, displayValue: "another-long-segment/frontend" },
    ]);
  });

  it("keeps long Bash field values whole and shortens only their display", () => {
    const description = `Run ${"the full integration suite ".repeat(4)}`;
    const env = {
      NODE_OPTIONS: "--max-old-space-size=8192",
      CI: "true",
      TEST_FILTER: "sync-and-watcher",
    };
    const envText =
      '{"NODE_OPTIONS":"--max-old-space-size=8192","CI":"true","TEST_FILTER":"sync-and-watcher"}';
    const meta = extractToolParamMeta("Bash", {
      command: "make test",
      description,
      env,
      timeout: 600000,
    });
    expect(meta).toEqual([
      {
        label: "description",
        value: description,
        displayValue: `${description.slice(0, 80)}\u2026`,
      },
      { label: "env", value: envText, displayValue: `${envText.slice(0, 80)}\u2026` },
      { label: "timeout", value: "600000" },
    ]);
  });

  it("has no Bash fields when only the command is set", () => {
    expect(extractToolParamMeta("Bash", { command: "ls -la" })).toBeNull();
  });

  it("extracts Skill name", () => {
    const meta = extractToolParamMeta("Skill", {
      skill: "commit",
    });
    expect(meta).toEqual([{ label: "skill", value: "commit" }]);
  });

  it("dispatches on category for Gemini read_file", () => {
    const meta = extractToolParamMeta("read_file", { file_path: "/src/main.go" }, "Read");
    expect(meta).toEqual([{ label: "file", value: "/src/main.go" }]);
  });

  it("dispatches on category for Gemini run_command", () => {
    const meta = extractToolParamMeta(
      "run_command",
      { command: "go test ./...", cwd: "/repo" },
      "Bash",
    );
    expect(meta).toEqual([{ label: "cwd", value: "/repo" }]);
  });

  it("dispatches on category for Gemini grep_search", () => {
    const meta = extractToolParamMeta("grep_search", { query: "TODO" }, "Grep");
    expect(meta).toEqual([{ label: "pattern", value: "TODO" }]);
  });

  it("falls back to toolName when category is empty string", () => {
    const meta = extractToolParamMeta("Read", { file_path: "/src/app.ts" }, "");
    expect(meta).toEqual([{ label: "file", value: "/src/app.ts" }]);
  });

  it("returns null for unknown tool with no matching params", () => {
    expect(extractToolParamMeta("CustomTool", { foo: "bar" })).toBeNull();
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

  it("keeps the canonical long path and provides a trailing display value", () => {
    const longPath = "/a".repeat(50);
    const meta = extractToolParamMeta("Read", {
      file_path: longPath,
    });
    expect(meta![0]!.value).toBe(longPath);
    expect(meta![0]!.displayValue).toBe("a/a");
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

  it("names the new path of a Codex Cloud change that renames its file", () => {
    const moved = extractToolParamMeta(
      "apply_patch",
      { path: "/repo/old.go", kind: { type: "update", move_path: "/repo/new.go" }, diff: "@@" },
      "Edit",
    );
    expect(moved).toEqual([
      { label: "file", value: "/repo/old.go" },
      { label: "moved to", value: "/repo/new.go" },
    ]);
    const inPlace = extractToolParamMeta(
      "apply_patch",
      { path: "/repo/a.go", kind: { type: "update", move_path: null }, diff: "@@" },
      "Edit",
    );
    expect(inPlace).toEqual([{ label: "file", value: "/repo/a.go" }]);
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
    expect(generateFallbackContent("Task", { prompt: "do stuff" })).toBeNull();
  });

  it("renders Cursor ApplyPatch patch text from input_json", () => {
    const patch = "@@ -1,1 +1,1 @@\n-old\n+new";

    expect(
      generateFallbackContent("Edit", {
        path: "src/app.ts",
        patch,
      }),
    ).toBe(patch);
  });

  it("shows diff for Edit tool", () => {
    const result = generateFallbackContent("Edit", {
      file_path: "/src/app.ts",
      old_string: "const x = 1;",
      new_string: "const x = 2;",
    });
    expect(result).toBe("@@ -1,1 +1,1 @@\n-const x = 1;\n+const x = 2;");
  });

  it("shows diff for Edit tool using pi old_str/new_str field names", () => {
    const result = generateFallbackContent("Edit", {
      path: "/src/app.ts",
      old_str: "const x = 1;",
      new_str: "const x = 2;",
    });
    expect(result).toBe("@@ -1,1 +1,1 @@\n-const x = 1;\n+const x = 2;");
  });

  it("shows only new_string when old_string is empty", () => {
    const result = generateFallbackContent("Edit", {
      file_path: "/src/app.ts",
      old_string: "",
      new_string: "const x = 1;",
    });
    expect(result).toBe("@@ -1,1 +1,1 @@\n-\n+const x = 1;");
  });

  it("shows diff for Edit tool using opencode camelCase field names", () => {
    const result = generateFallbackContent("Edit", {
      filePath: "/src/styles.css",
      oldString: ".foo { color: red; }",
      newString: ".foo { color: blue; }",
    });
    expect(result).toBe("@@ -1,1 +1,1 @@\n-.foo { color: red; }\n+.foo { color: blue; }");
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
    expect(result).toBe("insert after 248:18\n.new-class { color: red; }");
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
    expect(generateFallbackContent("Edit", { file_path: "/src/app.ts" })).toBeNull();
  });

  it("shows full Edit strings without truncation", () => {
    const long = "x".repeat(600);
    const result = generateFallbackContent("Edit", {
      old_string: long,
      new_string: "short",
    })!;
    // -prefix + full 600 chars
    const oldLine = result.split("\n")[1]!;
    expect(oldLine).toBe("-" + long);
  });

  it("shows Write content as all-additions diff", () => {
    const result = generateFallbackContent("Write", {
      file_path: "/src/new.ts",
      content: 'export const x = "hello";',
    });
    expect(result).toBe('@@ -0,0 +1,1 @@\n+export const x = "hello";');
  });

  it("shows full Write content without truncation", () => {
    const long = "line\n".repeat(200);
    const result = generateFallbackContent("Write", {
      file_path: "/src/big.ts",
      content: long,
    })!;
    expect(result).toContain("+line");
    expect(result.split("\n").length).toBe(202); // hunk + 200 lines + trailing empty
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
    expect(result).toBe("file_path: /src/app.ts\nlimit: 100");
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
    expect(result).toBe('arr: [1,2,3]\nobj: {"nested":true}');
  });

  it("returns null when params are all empty", () => {
    expect(generateFallbackContent("CustomTool", {})).toBeNull();
  });
});

describe("generateFallbackContent - Bash", () => {
  it("shows only the command; the other parameters are labeled fields", () => {
    const result = generateFallbackContent("Bash", {
      command: "npm test",
      description: "Run test suite",
      workdir: "/repo/frontend",
      timeout: 120000,
    });
    expect(result).toBe("npm test");
  });

  it("quotes an argument array so it can be pasted into a shell", () => {
    const result = generateFallbackContent("Bash", {
      command: ["bash", "-lc", "echo 'hi there'", ""],
    });
    expect(result).toBe(`bash -lc 'echo '\\''hi there'\\''' ''`);
  });

  it("shows full multiline heredoc command without truncation", () => {
    const heredoc = [
      "cd /project && PYTHONIOENCODING=utf-8 python - <<'PY'",
      "import json",
      'F="data.jsonl"',
      "rows=[json.loads(l) for l in open(F,encoding='utf-8')]",
      "for r in rows:",
      "    print(r)",
      "PY",
    ].join("\n");
    const result = generateFallbackContent("Bash", {
      command: heredoc,
      description: "Inspect data",
    });
    expect(result).toContain("import json");
    expect(result).toContain("for r in rows:");
    expect(result).not.toContain("…");
  });

  it("caps Bash commands at MAX_DIFF_LINES", () => {
    const longCommand = Array.from({ length: 250 }, (_, i) => `echo ${i}`).join("\n");
    const result = generateFallbackContent("Bash", {
      command: longCommand,
    })!;
    expect(result).toContain("lines total)");
    const lines = result.split("\n");
    expect(lines.length).toBeLessThanOrEqual(202);
  });

  it("handles run_command tool name (Gemini)", () => {
    const result = generateFallbackContent("run_command", {
      command: "go test ./...",
    });
    expect(result).toBe("go test ./...");
  });

  it("uses cmd param for codex exec_command", () => {
    const result = generateFallbackContent("Bash", {
      cmd: "ls -la /tmp",
    });
    expect(result).toBe("ls -la /tmp");
  });
});

describe("generateFallbackContent - generic no-truncation", () => {
  it("shows full long values without character truncation", () => {
    const longValue = "x".repeat(500);
    const result = generateFallbackContent("CustomTool", {
      data: longValue,
    })!;
    expect(result).toBe(`data: ${longValue}`);
    expect(result).not.toContain("…");
  });

  it("caps generic content at line count", () => {
    const longValue = Array.from({ length: 250 }, (_, i) => `line ${i}`).join("\n");
    const result = generateFallbackContent("CustomTool", {
      data: longValue,
    })!;
    expect(result).toContain("lines total)");
    const lines = result.split("\n");
    expect(lines.length).toBeLessThanOrEqual(202);
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
    expect(result).not.toContain("agent__intent");
    expect(result).toBe("npm test");
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

describe("toolCopyText", () => {
  it.each([
    [
      "a Bash command without its working directory",
      "shell_command",
      "Bash",
      { command: "python - <<'PY'\nprint(1)\nPY", cwd: "/repo", description: "Run it" },
      "python - <<'PY'\nprint(1)\nPY",
    ],
    ["a codex exec_command cmd", "exec_command", "Bash", { cmd: "ls", workdir: "/repo" }, "ls"],
    [
      "an edit's diff",
      "apply_patch",
      "Edit",
      { path: "/repo/a.go", kind: { type: "update" }, diff: "@@ -1 +1 @@\n-old\n+new\n" },
      "@@ -1 +1 @@\n-old\n+new\n",
    ],
    [
      "an edit built from old and new strings",
      "Edit",
      "Edit",
      { file_path: "/repo/a.go", old_string: "old", new_string: "new" },
      "@@ -1,1 +1,1 @@\n-old\n+new",
    ],
    [
      "an edit's path when it has no diff",
      "Edit",
      "Edit",
      { file_path: "/repo/a.go" },
      "/repo/a.go",
    ],
    [
      "a Pi edit's operations as JSON",
      "edit",
      "Edit",
      { path: "/repo/a.go", edits: [{ set_line: { anchor: "3:ab", new_text: "x" } }] },
      '{\n  "path": "/repo/a.go",\n  "edits": [\n    {\n      "set_line": {\n        "anchor": "3:ab",\n        "new_text": "x"\n      }\n    }\n  ]\n}',
    ],
    [
      "a RooCode replacement's path and content as JSON",
      "searchAndReplace",
      "Edit",
      { path: "/repo/a.go", content: "func a() {}" },
      '{\n  "path": "/repo/a.go",\n  "content": "func a() {}"\n}',
    ],
    [
      "a write's file content",
      "Write",
      "Write",
      { file_path: "/repo/a.txt", content: "one\ntwo" },
      "one\ntwo",
    ],
    [
      "an empty write's path",
      "Write",
      "Write",
      { file_path: "/repo/a.txt", content: "" },
      "/repo/a.txt",
    ],
    ["a read's path", "read_file", "Read", { file_path: "/repo/a.go", offset: 10 }, "/repo/a.go"],
    ["a search pattern", "Grep", "Grep", { pattern: "TODO", path: "/repo" }, "TODO"],
    ["a single text argument", "web_search", "Tool", { query: "zstd frames" }, "zstd frames"],
    [
      "other input as JSON",
      "mcp__notes__create",
      "Tool",
      { path: "/notes/a.md", tags: ["x"], agent__intent: "internal" },
      '{\n  "path": "/notes/a.md",\n  "tags": [\n    "x"\n  ]\n}',
    ],
  ] as const)("copies %s", (_name, toolName, category, params, want) => {
    expect(toolCopyText(toolName, category, { ...params })).toBe(want);
  });

  it("falls back to the tool name when the category has no rule", () => {
    expect(toolCopyText("Read", "", { file_path: "/repo/a.go", offset: 10 })).toBe("/repo/a.go");
  });

  it("leaves Task calls and empty input to the caller", () => {
    expect(toolCopyText("Task", "Task", { prompt: "do it" })).toBeNull();
    expect(toolCopyText("custom", "Other", { _i: "internal" })).toBeNull();
  });
});
