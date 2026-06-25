package parser

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestInferCursorSkillNameFromReadFile(t *testing.T) {
	path := writeTestSkill(t, "foo", "foo")

	_, _, toolCalls := extractAssistantContent([]string{
		"[Tool call] ReadFile",
		"  path=" + path,
	})

	require.Len(t, toolCalls, 1)
	assert.Equal(t, "ReadFile", toolCalls[0].ToolName)
	assert.Equal(t, "foo", toolCalls[0].SkillName)
}

func TestInferCursorSkillNameFromShellCommand(t *testing.T) {
	path := writeTestSkill(t, "foo", "foo")

	_, _, toolCalls := extractAssistantContent([]string{
		"[Tool call] Shell",
		"  command=cat " + path,
	})

	require.Len(t, toolCalls, 1)
	assert.Equal(t, "Shell", toolCalls[0].ToolName)
	assert.Equal(t, "foo", toolCalls[0].SkillName)
}

func TestInferCursorSkillNameIgnoresShellWriteCommand(t *testing.T) {
	path := writeTestSkill(t, "foo", "foo")

	_, _, toolCalls := extractAssistantContent([]string{
		"[Tool call] Shell",
		"  command=cp " + path + " /tmp/SKILL.md",
	})

	require.Len(t, toolCalls, 1)
	assert.Empty(t, toolCalls[0].SkillName)
}

func TestInferCursorSkillNameUsesFrontmatterName(t *testing.T) {
	path := writeTestSkill(t, "index", "data-analytics:index")

	_, _, toolCalls := extractAssistantContent([]string{
		"[Tool call] ReadFile",
		`  {"path":` + quoteJSON(t, path) + `}`,
	})

	require.Len(t, toolCalls, 1)
	assert.Equal(t, "data-analytics:index", toolCalls[0].SkillName)
}

func TestInferCursorSkillNameIgnoresDiscoveryAndNonSkillPaths(t *testing.T) {
	path := writeTestSkill(t, "foo", "foo")

	tests := []struct {
		name string
		line string
	}{
		{
			name: "glob discovery",
			line: "[Tool call] Glob\n  " +
				`{"target_directory":` + quoteJSON(t, filepath.Dir(path)) +
				`,"glob_pattern":"**/SKILL.md"}`,
		},
		{
			name: "non skill path",
			line: `[Tool call] ReadFile
  path=` + filepath.Join(t.TempDir(), "README.md"),
		},
		{
			name: "empty input",
			line: "[Tool call] ReadFile",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, toolCalls := extractAssistantContent(
				splitTestLines(tt.line),
			)
			require.Len(t, toolCalls, 1)
			assert.Empty(t, toolCalls[0].SkillName)
		})
	}
}

func TestInferCodexSkillNameFromReadCommands(t *testing.T) {
	path := writeTestSkill(t, "foo", "foo")

	for _, cmd := range []string{
		"cat " + path,
		"sed -n '1,220p' " + path,
		"head -40 " + path,
		"tail -40 " + path,
		"rg name " + path,
		"grep name " + path,
		"cd /tmp && sed -n '1,220p' " + path,
	} {
		t.Run(cmd, func(t *testing.T) {
			got := inferCodexSkillName(
				"exec_command",
				`{"cmd":`+quoteJSON(t, cmd)+`}`,
			)
			assert.Equal(t, "foo", got)
		})
	}
}

func TestInferCodexSkillNameIgnoresWriteCommands(t *testing.T) {
	path := writeTestSkill(t, "foo", "foo")

	for _, cmd := range []string{
		"cp " + path + " /tmp/SKILL.md",
		"mv " + path + " /tmp/SKILL.md",
		"mkdir -p " + filepath.Dir(path),
		"git add " + path,
		"sed -i '' 's/a/b/' " + path,
		"sed -ni 's/a/b/' " + path,
		"sed -Ei 's/a/b/' " + path,
		"echo hi && sed -i '' 's/a/b/' " + path,
		"cat > " + path,
	} {
		t.Run(cmd, func(t *testing.T) {
			got := inferCodexSkillName(
				"exec_command",
				`{"cmd":`+quoteJSON(t, cmd)+`}`,
			)
			assert.Empty(t, got)
		})
	}
}

func TestInferCodexSkillNameMixedWriteThenRead(t *testing.T) {
	// A write segment earlier in the command must not suppress a real
	// skill read in a later segment; each segment is classified on its
	// own leading verb rather than rejecting the whole command.
	path := writeTestSkill(t, "foo", "data-analytics:foo")

	for _, cmd := range []string{
		"mkdir -p out && cat " + path,
		"git add -A && sed -n '1,40p' " + path,
		"touch marker; grep name " + path,
	} {
		t.Run(cmd, func(t *testing.T) {
			got := inferCodexSkillName(
				"exec_command",
				`{"cmd":`+quoteJSON(t, cmd)+`}`,
			)
			assert.Equal(t, "data-analytics:foo", got)
		})
	}
}

func TestInferCodexSkillNameIgnoresGlobDiscovery(t *testing.T) {
	for _, cmd := range []string{
		"rg --files -g '**/SKILL.md'",
		"cat **/SKILL.md",
		"sed -n '1,40p' skills/*/SKILL.md",
		"grep name skills/?ndex/SKILL.md",
		"head -40 skills/[ab]/SKILL.md",
	} {
		t.Run(cmd, func(t *testing.T) {
			got := inferCodexSkillName(
				"exec_command",
				`{"cmd":`+quoteJSON(t, cmd)+`}`,
			)
			assert.Empty(t, got)
		})
	}
}

func TestInferCodexSkillNameBareSkillFileWithWorkdir(t *testing.T) {
	path := writeTestSkill(t, "data-analytics", "data-analytics:index")
	workdir := filepath.Dir(path)

	got := inferCodexSkillName(
		"exec_command",
		`{"cmd":`+quoteJSON(t, "cat SKILL.md")+
			`,"workdir":`+quoteJSON(t, workdir)+`}`,
	)
	assert.Equal(t, "data-analytics:index", got)
}

func TestInferCodexSkillNameBareSkillFileUsesFallbackCwd(t *testing.T) {
	path := writeTestSkill(t, "data-analytics", "data-analytics:index")
	cwd := filepath.Dir(path)

	got := inferCodexSkillNameWithBase(
		"exec_command",
		`{"cmd":`+quoteJSON(t, "sed -n '1,40p' SKILL.md")+`}`,
		cwd,
	)
	assert.Equal(t, "data-analytics:index", got)
}

func TestInferCodexSkillNameBareSkillPatternWithoutFileIgnored(t *testing.T) {
	// "grep SKILL.md notes.txt" uses SKILL.md as a search pattern, not
	// a file. With a workdir that holds no SKILL.md the bare token must
	// not be resolved to the workdir name and miscounted as usage.
	workdir := t.TempDir()

	got := inferCodexSkillName(
		"exec_command",
		`{"cmd":`+quoteJSON(t, "grep SKILL.md notes.txt")+
			`,"workdir":`+quoteJSON(t, workdir)+`}`,
	)
	assert.Empty(t, got)
}

func TestInferCodexSkillNameBareSkillSearchPatternIgnoredEvenWithFile(t *testing.T) {
	// grep/rg take the search pattern as their first operand, so a bare
	// "SKILL.md" there is the pattern, not a file to read. It must not be
	// inferred even when the workdir holds a readable SKILL.md.
	path := writeTestSkill(t, "data-analytics", "data-analytics:index")
	workdir := filepath.Dir(path)

	for _, cmd := range []string{
		"grep SKILL.md notes.txt",
		"rg SKILL.md",
		"rg SKILL.md notes.txt",
		"rg -t md SKILL.md",            // SKILL.md is the pattern; md is -t's value
		"grep -A 3 SKILL.md notes.txt", // SKILL.md is the pattern; 3 is -A's value
		"grep -e SKILL.md notes.txt",   // SKILL.md is the -e pattern value
	} {
		t.Run(cmd, func(t *testing.T) {
			got := inferCodexSkillName(
				"exec_command",
				`{"cmd":`+quoteJSON(t, cmd)+
					`,"workdir":`+quoteJSON(t, workdir)+`}`,
			)
			assert.Empty(t, got)
		})
	}
}

func TestInferCodexSkillNameSearchCommandReadsFileOperand(t *testing.T) {
	// When a pattern precedes SKILL.md (or -e/-f supplies the pattern),
	// SKILL.md is a file operand and a workdir-local read is inferred.
	path := writeTestSkill(t, "data-analytics", "data-analytics:index")
	workdir := filepath.Dir(path)

	for _, cmd := range []string{
		"grep name SKILL.md",
		"rg pattern SKILL.md",
		"cat SKILL.md | grep name",
		"grep -e foo SKILL.md",
		"grep -i name SKILL.md",
	} {
		t.Run(cmd, func(t *testing.T) {
			got := inferCodexSkillName(
				"exec_command",
				`{"cmd":`+quoteJSON(t, cmd)+
					`,"workdir":`+quoteJSON(t, workdir)+`}`,
			)
			assert.Equal(t, "data-analytics:index", got)
		})
	}
}

func TestInferCodexSkillNameSearchQuotedPatternIgnored(t *testing.T) {
	// A quoted multi-word search pattern containing SKILL.md must not be
	// split into fields and read as a file, even with a workdir SKILL.md.
	path := writeTestSkill(t, "data-analytics", "data-analytics:index")
	workdir := filepath.Dir(path)

	for _, cmd := range []string{
		`grep "foo SKILL.md" notes.txt`,
		`rg 'see SKILL.md for details'`,
		`grep -e "read the SKILL.md" notes.txt`,
	} {
		t.Run(cmd, func(t *testing.T) {
			got := inferCodexSkillName(
				"exec_command",
				`{"cmd":`+quoteJSON(t, cmd)+
					`,"workdir":`+quoteJSON(t, workdir)+`}`,
			)
			assert.Empty(t, got)
		})
	}
}

func TestInferCodexSkillNameIgnoresNonReadSegments(t *testing.T) {
	// A read command elsewhere in the line must not let SKILL.md in an
	// unrelated segment (echo, ls, ...) be inferred as a read.
	path := writeTestSkill(t, "data-analytics", "data-analytics:index")
	workdir := filepath.Dir(path)

	for _, cmd := range []string{
		"grep foo notes.txt && echo SKILL.md",
		"cat notes.txt && echo see SKILL.md",
		"grep foo notes.txt; ls SKILL.md",
	} {
		t.Run(cmd, func(t *testing.T) {
			got := inferCodexSkillName(
				"exec_command",
				`{"cmd":`+quoteJSON(t, cmd)+
					`,"workdir":`+quoteJSON(t, workdir)+`}`,
			)
			assert.Empty(t, got)
		})
	}
}

func TestTokenizeCommand(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want []string
	}{
		{
			// Backslashes are literal so Windows paths survive intact;
			// a full shell lexer such as shlex would consume them.
			name: "preserves backslash path",
			cmd:  `cat C:\Users\me\skills\foo\SKILL.md`,
			want: []string{"cat", `C:\Users\me\skills\foo\SKILL.md`},
		},
		{
			name: "double-quoted argument stays one token",
			cmd:  `grep "foo SKILL.md" notes.txt`,
			want: []string{"grep", "foo SKILL.md", "notes.txt"},
		},
		{
			name: "single-quoted argument stays one token",
			cmd:  `rg 'see SKILL.md for'`,
			want: []string{"rg", "see SKILL.md for"},
		},
		{
			name: "drops redirect target attached to operator",
			cmd:  "cat foo >skills/x/SKILL.md",
			want: []string{"cat", "foo"},
		},
		{
			name: "drops redirect operator attached to source",
			cmd:  "cat file> out",
			want: []string{"cat", "file"},
		},
		{
			name: "drops space-separated redirect target",
			cmd:  "cat a > b",
			want: []string{"cat", "a"},
		},
		{
			name: "keeps quoted redirect char as content",
			cmd:  `grep ">" f`,
			want: []string{"grep", ">", "f"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tokenizeCommand(tt.cmd))
		})
	}
}

func TestInferCodexSkillNameIgnoresRedirectTargets(t *testing.T) {
	// A redirect destination (>, >>, 2> ...) is written, not read, so a
	// SKILL.md target must not be inferred even with a workdir file. The
	// operator may be attached to the target (>SKILL.md) or to the source
	// token (file>), so whitespace around it cannot be relied upon.
	path := writeTestSkill(t, "data-analytics", "data-analytics:index")
	workdir := filepath.Dir(path)

	for _, cmd := range []string{
		"cat foo > SKILL.md",
		"grep name file > SKILL.md",
		"cat foo >> SKILL.md",
		"cat foo 2> SKILL.md",
		"cat foo >SKILL.md",
		"cat foo >>SKILL.md",
		"cat file> SKILL.md",
		"grep name file 2>SKILL.md",
		"cat foo >skills/data-analytics/SKILL.md",
	} {
		t.Run(cmd, func(t *testing.T) {
			got := inferCodexSkillName(
				"exec_command",
				`{"cmd":`+quoteJSON(t, cmd)+
					`,"workdir":`+quoteJSON(t, workdir)+`}`,
			)
			assert.Empty(t, got)
		})
	}
}

func TestInferCodexSkillNameReadsFileDespiteQuotedRedirectChar(t *testing.T) {
	// A quoted ">" is a literal search pattern, not a redirect operator,
	// so the SKILL.md file operand is still read.
	path := writeTestSkill(t, "data-analytics", "data-analytics:index")

	got := inferCodexSkillName(
		"exec_command",
		`{"cmd":`+quoteJSON(t, `grep ">" `+path)+`}`,
	)
	assert.Equal(t, "data-analytics:index", got)
}

func TestInferCodexSkillNameReadsSourceDespiteRedirect(t *testing.T) {
	// SKILL.md is the input being read; output is redirected elsewhere,
	// so the read is still inferred.
	path := writeTestSkill(t, "data-analytics", "data-analytics:index")

	got := inferCodexSkillName(
		"exec_command",
		`{"cmd":`+quoteJSON(t, "cat "+path+" > out.txt")+`}`,
	)
	assert.Equal(t, "data-analytics:index", got)
}

func TestInferCodexSkillNameSearchCommandStillReadsPathOperand(t *testing.T) {
	// A path-qualified SKILL.md operand to grep/rg is a real file read
	// (the pattern is a separate token), so it is still inferred.
	path := writeTestSkill(t, "data-analytics", "data-analytics:index")

	got := inferCodexSkillName(
		"exec_command",
		`{"cmd":`+quoteJSON(t, "grep name "+path)+`}`,
	)
	assert.Equal(t, "data-analytics:index", got)
}

func TestParseCodexSessionInfersSkillName(t *testing.T) {
	path := writeTestSkill(t, "index", "data-analytics:index")
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("skill-read", "/tmp", "user", tsEarly),
		testjsonl.CodexMsgJSON("user", "use the dashboard skill", tsEarlyS1),
		testjsonl.CodexFunctionCallArgsJSON("exec_command", map[string]any{
			"cmd": "sed -n '1,220p' '" + path + "'",
		}, tsEarlyS5),
	)

	_, msgs := runCodexParserTest(t, "skill-read.jsonl", content, false)

	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "data-analytics:index", msgs[1].ToolCalls[0].SkillName)
}

func TestParseCodexSessionInfersSkillNameFromSessionCwd(t *testing.T) {
	path := writeTestSkill(t, "index", "data-analytics:index")
	cwd := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("skill-read", cwd, "user", tsEarly),
		testjsonl.CodexMsgJSON("user", "use the dashboard skill", tsEarlyS1),
		testjsonl.CodexFunctionCallArgsJSON("exec_command", map[string]any{
			"cmd": "sed -n '1,220p' skills/index/SKILL.md",
		}, tsEarlyS5),
	)

	_, msgs := runCodexParserTest(t, "skill-read.jsonl", content, false)

	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "data-analytics:index", msgs[1].ToolCalls[0].SkillName)
}

func TestParseCodexSessionFromInfersSkillNameFromSeededCwd(t *testing.T) {
	path := writeTestSkill(t, "index", "data-analytics:index")
	cwd := filepath.Dir(filepath.Dir(filepath.Dir(path)))

	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("inc-skill", cwd, "codex_cli_rs", tsEarly),
		testjsonl.CodexMsgJSON("user", "use the dashboard skill", tsEarlyS1),
	)
	file := createTestFile(t, "incremental-skill.jsonl", initial)
	_, msgs, err := parseCodexTestSession(t, file, "local", false)
	require.NoError(t, err)

	info, err := os.Stat(file)
	require.NoError(t, err)
	offset := info.Size()

	appended := testjsonl.CodexFunctionCallArgsJSON(
		"exec_command", map[string]any{
			"cmd": "sed -n '1,220p' skills/index/SKILL.md",
		}, tsLateS5)
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	newMsgs, _, _, err := parseCodexTestSessionFrom(t, file, offset, len(msgs), false)
	require.NoError(t, err)
	require.Len(t, newMsgs, 1)
	require.Len(t, newMsgs[0].ToolCalls, 1)
	assert.Equal(t, "data-analytics:index", newMsgs[0].ToolCalls[0].SkillName)
}

func TestExtractTextContentInfersCursorJSONLSkillName(t *testing.T) {
	path := writeTestSkill(t, "planning-and-task-breakdown", "planning-and-task-breakdown")
	content := gjson.Parse(
		`[{"type":"tool_use","id":"tu_read","name":"Read","input":{"path":` +
			quoteJSON(t, path) + `}}]`,
	)

	_, _, _, _, toolCalls, _ := ExtractTextContent(content)

	require.Len(t, toolCalls, 1)
	assert.Equal(t, "Read", toolCalls[0].ToolName)
	assert.Equal(t, "planning-and-task-breakdown", toolCalls[0].SkillName)
}

func TestExtractTextContentInfersCursorJSONLSkillNameFromFrontmatter(t *testing.T) {
	path := writeTestSkill(t, "index", "data-analytics:index")
	content := gjson.Parse(
		`[{"type":"tool_use","id":"tu_read_file","name":"ReadFile","input":{"path":` +
			quoteJSON(t, path) + `}}]`,
	)

	_, _, _, _, toolCalls, _ := ExtractTextContent(content)

	require.Len(t, toolCalls, 1)
	assert.Equal(t, "ReadFile", toolCalls[0].ToolName)
	assert.Equal(t, "data-analytics:index", toolCalls[0].SkillName)
}

func TestExtractTextContentInfersSkillNameFromPathWithSpaces(t *testing.T) {
	path := writeTestSkill(t, "my index", "data-analytics:index")
	require.Contains(t, path, " ")

	content := gjson.Parse(
		`[{"type":"tool_use","id":"tu_read","name":"Read","input":{"path":` +
			quoteJSON(t, path) + `}}]`,
	)

	_, _, _, _, toolCalls, _ := ExtractTextContent(content)

	require.Len(t, toolCalls, 1)
	assert.Equal(t, "data-analytics:index", toolCalls[0].SkillName)
}

func TestExtractTextContentInfersCursorJSONLSkillNameFromShellRead(t *testing.T) {
	path := writeTestSkill(t, "qa", "qa")
	content := gjson.Parse(
		`[{"type":"tool_use","id":"tu_shell","name":"Shell","input":{"command":` +
			quoteJSON(t, "cd /tmp && sed -n '1,120p' "+path) + `}}]`,
	)

	_, _, _, _, toolCalls, _ := ExtractTextContent(content)

	require.Len(t, toolCalls, 1)
	assert.Equal(t, "Shell", toolCalls[0].ToolName)
	assert.Equal(t, "qa", toolCalls[0].SkillName)
}

func TestExtractTextContentDoesNotInferCursorJSONLNonUsage(t *testing.T) {
	path := writeTestSkill(t, "foo", "foo")
	templatePath := filepath.Join(t.TempDir(), "SKILL.md.tmpl")
	require.NoError(t, os.WriteFile(templatePath, []byte("template"), 0o644))

	tests := []struct {
		name     string
		toolName string
		input    string
	}{
		{
			name:     "glob discovery",
			toolName: "Glob",
			input:    `{"glob_pattern":"**/SKILL.md"}`,
		},
		{
			name:     "write skill file",
			toolName: "Write",
			input:    `{"path":` + quoteJSON(t, path) + `,"contents":"---"}`,
		},
		{
			name:     "str replace skill file",
			toolName: "StrReplace",
			input:    `{"path":` + quoteJSON(t, path) + `,"old_string":"a","new_string":"b"}`,
		},
		{
			name:     "apply patch skill file",
			toolName: "ApplyPatch",
			input:    `{"path":` + quoteJSON(t, path) + `,"patch":"*** Begin Patch"}`,
		},
		{
			name:     "template file",
			toolName: "Read",
			input:    `{"path":` + quoteJSON(t, templatePath) + `}`,
		},
		{
			name:     "shell write command",
			toolName: "Shell",
			input:    `{"command":` + quoteJSON(t, "cp "+path+" /tmp/SKILL.md") + `}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := gjson.Parse(
				`[{"type":"tool_use","id":"tu","name":` +
					quoteJSON(t, tt.toolName) + `,"input":` + tt.input + `}]`,
			)

			_, _, _, _, toolCalls, _ := ExtractTextContent(content)

			require.Len(t, toolCalls, 1)
			assert.Empty(t, toolCalls[0].SkillName)
		})
	}
}

func TestInferCodexSkillNameResolvesRelativePathAgainstWorkdir(t *testing.T) {
	path := writeTestSkill(t, "index", "data-analytics:index")
	workdir := filepath.Dir(filepath.Dir(filepath.Dir(path)))

	got := inferCodexSkillName(
		"exec_command",
		`{"cmd":`+quoteJSON(t, "sed -n '1,220p' skills/index/SKILL.md")+
			`,"workdir":`+quoteJSON(t, workdir)+`}`,
	)

	assert.Equal(t, "data-analytics:index", got)
}

func TestInferCodexSkillNameWorkdirOverridesFallbackBase(t *testing.T) {
	path := writeTestSkill(t, "index", "data-analytics:index")
	workdir := filepath.Dir(filepath.Dir(filepath.Dir(path)))

	got := inferCodexSkillNameWithBase(
		"exec_command",
		`{"cmd":`+quoteJSON(t, "cat skills/index/SKILL.md")+
			`,"workdir":`+quoteJSON(t, workdir)+`}`,
		"/nonexistent/fallback",
	)

	assert.Equal(t, "data-analytics:index", got)
}

func TestInferCodexSkillNameUsesFallbackBaseWhenNoWorkdir(t *testing.T) {
	path := writeTestSkill(t, "index", "data-analytics:index")
	fallback := filepath.Dir(filepath.Dir(filepath.Dir(path)))

	got := inferCodexSkillNameWithBase(
		"exec_command",
		`{"cmd":`+quoteJSON(t, "cat skills/index/SKILL.md")+`}`,
		fallback,
	)

	assert.Equal(t, "data-analytics:index", got)
}

func TestInferCodexSkillNameRelativePathNoWorkdirUsesParentFallback(t *testing.T) {
	// Frontmatter name ("data-analytics:index") differs from the
	// parent dir ("index"). Without a workdir the relative path
	// cannot be resolved, so the frontmatter read is skipped and
	// the parent-dir fallback is used instead of reading an
	// unrelated SKILL.md under the process cwd.
	writeTestSkill(t, "index", "data-analytics:index")

	got := inferCodexSkillName(
		"exec_command",
		`{"cmd":`+quoteJSON(t, "cat skills/index/SKILL.md")+`}`,
	)

	assert.Equal(t, "index", got)
}

func TestExpandSkillHome(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare tilde", "~", home},
		{"tilde slash", "~/.claude/skills/foo/SKILL.md",
			filepath.Join(home, ".claude/skills/foo/SKILL.md")},
		{"absolute unchanged", "/abs/SKILL.md", "/abs/SKILL.md"},
		{"relative unchanged", "skills/foo/SKILL.md", "skills/foo/SKILL.md"},
		{"tilde user not expanded", "~bob/SKILL.md", "~bob/SKILL.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, expandSkillHome(tt.in))
		})
	}
}

func TestResolveSkillPath(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	absSkillPath := filepath.Join(t.TempDir(), "a", "b", "SKILL.md")
	baseDir := filepath.Join(t.TempDir(), "repo")
	relativeSkillPath := filepath.Join("s", "SKILL.md")

	tests := []struct {
		name         string
		path         string
		baseDir      string
		wantPath     string
		wantReadable bool
	}{
		{"absolute", absSkillPath, "", absSkillPath, true},
		{"tilde expands", "~/s/SKILL.md", "",
			filepath.Join(home, "s/SKILL.md"), true},
		{"relative joined to base", relativeSkillPath, baseDir,
			filepath.Join(baseDir, relativeSkillPath), true},
		{"relative no base", relativeSkillPath, "", relativeSkillPath, false},
		{"relative with relative base", relativeSkillPath, "rel",
			relativeSkillPath, false},
		{"tilde base expands", relativeSkillPath, "~/repo",
			filepath.Join(home, "repo", relativeSkillPath), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath, gotReadable := resolveSkillPath(tt.path, tt.baseDir)
			assert.Equal(t, tt.wantPath, gotPath)
			assert.Equal(t, tt.wantReadable, gotReadable)
		})
	}
}

func TestSkillNameFromFrontmatterBoundsReadSize(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "huge")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "SKILL.md")
	// Frontmatter terminator sits past the read cap, so the name
	// cannot be parsed and resolution falls back to the parent dir.
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("description: ")
	sb.WriteString(strings.Repeat("x", maxSkillFrontmatterSize))
	sb.WriteString("\nname: too-far\n---\n")
	require.NoError(t, os.WriteFile(path, []byte(sb.String()), 0o644))

	assert.Equal(t, "huge", skillNameFromPath(path, ""))
}

func writeTestSkill(t *testing.T, folder, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "skills", folder)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "SKILL.md")
	content := "---\nname: " + name + "\ndescription: Test skill\n---\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func splitTestLines(s string) []string {
	return strings.Split(s, "\n")
}

func quoteJSON(t *testing.T, s string) string {
	t.Helper()
	return strconv.Quote(s)
}
