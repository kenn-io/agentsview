package parser

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/testjsonl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetProjectName(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		want string
	}{
		{"simple name", "my-project", "my_project"},
		{"encoded path with code",
			"-Users-alice-code-my-app", "my_app"},
		{"encoded path with projects",
			"-Users-alice-projects-api-server", "api_server"},
		{"encoded path with repos",
			"-home-user-repos-frontend", "frontend"},
		{"encoded path without marker",
			"-Users-alice", "alice"},
		{"empty", "", ""},
		{"no prefix", "plain_name", "plain_name"},
		{"with src marker",
			"-Users-alice-src-my-lib", "my_lib"},
		{"multi-word after marker",
			"-Users-alice-code-my-cool-project", "my_cool_project"},
		{"deeply nested",
			"-Users-alice-code-org-team-repo", "org_team_repo"},
		{"unicode components",
			"-Users-alice-code-café-app", "café_app"},
		{"trailing dash",
			"-Users-alice-code-myapp-", "myapp_"},
		{"double dashes",
			"-Users-alice-code--my-app", "_my_app"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetProjectName(tt.dir)
			assert.Equalf(t, tt.want, got, "GetProjectName(%q)", tt.dir)
		})
	}
}

func TestExtractProjectFromCwd(t *testing.T) {
	tests := []struct {
		cwd  string
		want string
	}{
		{cwd: "/Users/alice/code/my-app", want: "my_app"},
		{cwd: "/home/user/projects/api-server", want: "api_server"},
		{cwd: ""},
		{cwd: "/"},
		{cwd: "."},
		{cwd: ".."},
	}

	// Windows drive-letter paths are normalized cross-platform.
	tests = append(tests, []struct{ cwd, want string }{
		{cwd: `C:\Users\me\my-app`, want: "my_app"},
		{cwd: `D:\projects\frontend`, want: "frontend"},
	}...)
	// Mixed path: on Windows filepath treats \ as separator,
	// on POSIX it is a literal filename character.
	if runtime.GOOS == "windows" {
		tests = append(tests, struct{ cwd, want string }{
			cwd: `/mixed\path/to\project`, want: "project",
		})
	} else {
		tests = append(tests, struct{ cwd, want string }{
			cwd: `/mixed\path/to\project`,
		})
	}

	for _, tt := range tests {
		t.Run(tt.cwd, func(t *testing.T) {
			got := ExtractProjectFromCwd(tt.cwd)
			assert.Equalf(t, tt.want, got, "ExtractProjectFromCwd(%q)", tt.cwd)
		})
	}
}

func TestNeedsProjectReparse(t *testing.T) {
	tests := []struct {
		project string
		want    bool
	}{
		{"my_project", false},
		{"_Users_alice_code_app", true},
		{"_home_user_project", true},
		{"_private_var_folders", true},
		{"good_project_var_folders_ok", true},
		{"good_project", false},
		{"_var_folders_xx_temp", true},
		{"_private_tmp_build", true},
		{"_tmp_workspace", true},
		{"normal_var_project", false},
	}

	for _, tt := range tests {
		t.Run(tt.project, func(t *testing.T) {
			got := NeedsProjectReparse(tt.project)
			assert.Equalf(t, tt.want, got, "NeedsProjectReparse(%q)", tt.project)
		})
	}
}

func TestExtractTextContent(t *testing.T) {
	tests := []struct {
		name          string
		json          string
		wantText      string
		wantThink     bool
		wantToolUse   bool
		wantToolCalls []ParsedToolCall
	}{
		{
			"plain string",
			`"Hello world"`,
			"Hello world", false, false, nil,
		},
		{
			"text block array",
			`[{"type":"text","text":"Hi"}]`,
			"Hi", false, false, nil,
		},
		{
			"thinking block",
			`[{"type":"thinking","thinking":"Let me think..."}]`,
			"[Thinking]\nLet me think...\n[/Thinking]", true, false, nil,
		},
		{
			"tool_use block",
			`[{"type":"tool_use","name":"Read","input":{"file_path":"test.go"}}]`,
			"[Read: test.go]", false, true,
			[]ParsedToolCall{{ToolName: "Read", Category: "Read"}},
		},
		{
			"mixed blocks",
			`[{"type":"text","text":"Looking at"},{"type":"tool_use","name":"Bash","input":{"command":"ls","description":"list files"}}]`,
			"Looking at\n[Bash: list files]\n$ ls", false, true,
			[]ParsedToolCall{{ToolName: "Bash", Category: "Bash"}},
		},
		{
			"multiple tool_use blocks",
			`[{"type":"tool_use","name":"Read","input":{"file_path":"a.go"}},{"type":"tool_use","name":"Grep","input":{"pattern":"TODO"}}]`,
			"[Read: a.go]\n[Grep: TODO]", false, true,
			[]ParsedToolCall{
				{ToolName: "Read", Category: "Read"},
				{ToolName: "Grep", Category: "Grep"},
			},
		},
		{
			"tool_use with id and input",
			`[{"type":"tool_use","id":"toolu_123","name":"Read","input":{"file_path":"main.go"}}]`,
			"[Read: main.go]", false, true,
			[]ParsedToolCall{{ToolUseID: "toolu_123", ToolName: "Read", Category: "Read", InputJSON: `{"file_path":"main.go"}`}},
		},
		{
			"Skill tool extracts skill_name",
			`[{"type":"tool_use","id":"toolu_456","name":"Skill","input":{"skill":"superpowers:brainstorming"}}]`,
			"[Skill: superpowers:brainstorming]", false, true,
			[]ParsedToolCall{{ToolUseID: "toolu_456", ToolName: "Skill", Category: "Tool", InputJSON: `{"skill":"superpowers:brainstorming"}`, SkillName: "superpowers:brainstorming"}},
		},
		{
			"Amp skill tool extracts skill_name",
			`[{"type":"tool_use","id":"toolu_789","name":"skill","input":{"name":"frontend-design"}}]`,
			"[Skill: frontend-design]", false, true,
			[]ParsedToolCall{{ToolUseID: "toolu_789", ToolName: "skill", Category: "Tool", InputJSON: `{"name":"frontend-design"}`, SkillName: "frontend-design"}},
		},
		{
			"Amp skill tool supports legacy input.skill",
			`[{"type":"tool_use","id":"toolu_790","name":"skill","input":{"skill":"legacy-skill"}}]`,
			"[Skill: legacy-skill]", false, true,
			[]ParsedToolCall{{ToolUseID: "toolu_790", ToolName: "skill", Category: "Tool", InputJSON: `{"skill":"legacy-skill"}`, SkillName: "legacy-skill"}},
		},
		{
			"Amp skill tool falls back to input.name when input.skill is empty",
			`[{"type":"tool_use","id":"toolu_791","name":"skill","input":{"skill":"","name":"frontend-design"}}]`,
			"[Skill: frontend-design]", false, true,
			[]ParsedToolCall{{ToolUseID: "toolu_791", ToolName: "skill", Category: "Tool", InputJSON: `{"skill":"","name":"frontend-design"}`, SkillName: "frontend-design"}},
		},
		{
			"tool_use with empty name",
			`[{"type":"tool_use","name":"","input":{}}]`,
			"[Tool: ]", false, true, nil,
		},
		{
			"empty array",
			`[]`,
			"", false, false, nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := gjson.Parse(tt.json)
			text, _, hasThinking, hasToolUse, tcs, _ :=
				ExtractTextContent(result)
			assert.Equal(t, tt.wantText, text, "text")
			assert.Equal(t, tt.wantThink, hasThinking, "hasThinking")
			assert.Equal(t, tt.wantToolUse, hasToolUse, "hasToolUse")
			assertToolCalls(t, tcs, tt.wantToolCalls)
		})
	}
}

func TestExtractTextContent_AmpSkillNameExtraction(t *testing.T) {
	result := gjson.Parse(
		`[{"type":"tool_use","id":"toolu_amp_skill","name":"skill","input":{"name":"walkthrough"}}]`,
	)

	text, _, hasThinking, hasToolUse, toolCalls, toolResults :=
		ExtractTextContent(result)

	require.Equal(t, "[Skill: walkthrough]", text, "text")
	require.False(t, hasThinking, "hasThinking")
	require.True(t, hasToolUse, "hasToolUse")
	require.Empty(t, toolResults, "toolResults")
	require.Len(t, toolCalls, 1, "toolCalls")

	got := toolCalls[0]
	assert.Equal(t, "toolu_amp_skill", got.ToolUseID, "ToolUseID")
	assert.Equal(t, "skill", got.ToolName, "ToolName")
	assert.Equal(t, "Tool", got.Category, "Category")
	assert.Equal(t, "walkthrough", got.SkillName, "SkillName")
	assert.Equal(t, `{"name":"walkthrough"}`, got.InputJSON, "InputJSON")
}

func TestExtractToolResults(t *testing.T) {
	tests := []struct {
		name        string
		json        string
		wantResults []ParsedToolResult
	}{
		{
			"no tool_result blocks",
			`[{"type":"text","text":"hello"}]`,
			nil,
		},
		{
			"single tool_result",
			`[{"type":"tool_result","tool_use_id":"toolu_123","content":"file contents here"}]`,
			[]ParsedToolResult{{ToolUseID: "toolu_123", ContentLength: 18, ContentRaw: `"file contents here"`}},
		},
		{
			"tool_result with array content",
			`[{"type":"tool_result","tool_use_id":"toolu_456","content":[{"type":"text","text":"output data"}]}]`,
			[]ParsedToolResult{{ToolUseID: "toolu_456", ContentLength: 11, ContentRaw: `[{"type":"text","text":"output data"}]`}},
		},
		{
			"multiple tool_results",
			`[{"type":"tool_result","tool_use_id":"toolu_1","content":"abc"},{"type":"tool_result","tool_use_id":"toolu_2","content":"defgh"}]`,
			[]ParsedToolResult{
				{ToolUseID: "toolu_1", ContentLength: 3, ContentRaw: `"abc"`},
				{ToolUseID: "toolu_2", ContentLength: 5, ContentRaw: `"defgh"`},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := gjson.Parse(tt.json)
			_, _, _, _, _, trs := ExtractTextContent(result)
			require.Len(t, trs, len(tt.wantResults), "tool_results count")
			for i := range tt.wantResults {
				assert.Equalf(t, tt.wantResults[i].ToolUseID, trs[i].ToolUseID,
					"[%d].ToolUseID", i)
				assert.Equalf(t, tt.wantResults[i].ContentLength, trs[i].ContentLength,
					"[%d].ContentLength", i)
				assert.Equalf(t, tt.wantResults[i].ContentRaw, trs[i].ContentRaw,
					"[%d].ContentRaw", i)
			}
		})
	}
}

func TestDecodeContent(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"string content", `"file contents here"`, "file contents here"},
		{"array content", `[{"type":"text","text":"output data"}]`, "output data"},
		{"multiple array blocks", `[{"type":"text","text":"foo"},{"type":"text","text":"bar"}]`, "foobar"},
		{"empty raw", "", ""},
		{"non-text array block ignored", `[{"type":"image"}]`, ""},
		{
			"iFlow object with nested output",
			`{"responseParts":{"functionResponse":{"response":{"output":"hello world"}}}}`,
			"hello world",
		},
		{
			"iFlow object without nested output",
			`{"other":"data"}`,
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DecodeContent(tt.raw)
			assert.Equalf(t, tt.want, got, "DecodeContent(%q)", tt.raw)
		})
	}
}

func TestExtractTextContent_IflowToolResult(t *testing.T) {
	// iFlow tool results use an object with nested output.
	content := `[{
		"type":"tool_result",
		"tool_use_id":"tu_123",
		"content":{"responseParts":{"functionResponse":{"response":{"output":"result text"}}}}
	}]`
	_, _, _, _, _, trs := ExtractTextContent(gjson.Parse(content))
	require.Len(t, trs, 1, "expected 1 tool result")
	tr := trs[0]
	assert.Equal(t, "tu_123", tr.ToolUseID, "ToolUseID")
	assert.Equal(t, len("result text"), tr.ContentLength, "ContentLength")
	decoded := DecodeContent(tr.ContentRaw)
	assert.Equal(t, "result text", decoded, "DecodeContent")

	// Object without nested output: both length and decode
	// should be zero/empty.
	noOutput := `[{
		"type":"tool_result",
		"tool_use_id":"tu_456",
		"content":{"other":"data"}
	}]`
	_, _, _, _, _, trs2 := ExtractTextContent(gjson.Parse(noOutput))
	require.Len(t, trs2, 1, "expected 1 tool result")
	assert.Zero(t, trs2[0].ContentLength, "ContentLength")
	assert.Empty(t, DecodeContent(trs2[0].ContentRaw), "DecodeContent")
}

func TestFormatToolUseVariants(t *testing.T) {
	tests := []struct {
		toolName string
		json     string
		want     string
	}{
		{
			"Read",
			`{"type":"tool_use","name":"Read","input":{"file_path":"main.go"}}`,
			"[Read: main.go]",
		},
		{
			"Glob",
			`{"type":"tool_use","name":"Glob","input":{"pattern":"*.ts","path":"src"}}`,
			"[Glob: *.ts in src]",
		},
		{
			"Glob default path",
			`{"type":"tool_use","name":"Glob","input":{"pattern":"*.ts"}}`,
			"[Glob: *.ts in .]",
		},
		{
			"Grep",
			`{"type":"tool_use","name":"Grep","input":{"pattern":"TODO"}}`,
			"[Grep: TODO]",
		},
		{
			"Edit",
			`{"type":"tool_use","name":"Edit","input":{"file_path":"config.go"}}`,
			"[Edit: config.go]",
		},
		{
			"Write",
			`{"type":"tool_use","name":"Write","input":{"file_path":"new.go"}}`,
			"[Write: new.go]",
		},
		{
			"Bash with description",
			`{"type":"tool_use","name":"Bash","input":{"command":"go test ./...","description":"run tests"}}`,
			"[Bash: run tests]\n$ go test ./...",
		},
		{
			"Bash without description",
			`{"type":"tool_use","name":"Bash","input":{"command":"ls"}}`,
			"[Bash]\n$ ls",
		},
		{
			"Task",
			`{"type":"tool_use","name":"Task","input":{"description":"explore","subagent_type":"Explore"}}`,
			"[Task: explore (Explore)]",
		},
		{
			"Agent",
			`{"type":"tool_use","name":"Agent","input":{"description":"explore","subagent_type":"Explore"}}`,
			"[Task: explore (Explore)]",
		},
		{
			"EnterPlanMode",
			`{"type":"tool_use","name":"EnterPlanMode","input":{}}`,
			"[Entering Plan Mode]",
		},
		{
			"ExitPlanMode",
			`{"type":"tool_use","name":"ExitPlanMode","input":{}}`,
			"[Exiting Plan Mode]",
		},
		{
			"Unknown tool",
			`{"type":"tool_use","name":"CustomTool","input":{}}`,
			"[Tool: CustomTool]",
		},
		{
			"AskUserQuestion",
			`{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[{"question":"Which approach?","options":[{"label":"A","description":"First option"},{"label":"B","description":"Second option"}]}]}}`,
			"[Question: AskUserQuestion]\n  Which approach?\n    - A: First option\n    - B: Second option",
		},
		{
			"TodoWrite",
			`{"type":"tool_use","name":"TodoWrite","input":{"todos":[{"content":"Fix bug","status":"completed"},{"content":"Write tests","status":"in_progress"},{"content":"Deploy","status":"pending"}]}}`,
			"[Todo List]\n  ✓ Fix bug\n  → Write tests\n  ○ Deploy",
		},
		{
			"TodoWrite unknown status",
			`{"type":"tool_use","name":"TodoWrite","input":{"todos":[{"content":"Something","status":"unknown"}]}}`,
			"[Todo List]\n  ○ Something",
		},
		{
			"Skill",
			`{"type":"tool_use","name":"Skill","input":{"skill":"superpowers:brainstorming"}}`,
			"[Skill: superpowers:brainstorming]",
		},
		{
			"Skill with args",
			`{"type":"tool_use","name":"Skill","input":{"skill":"commit","args":"-m 'Fix bug'"}}`,
			"[Skill: commit]",
		},
		{
			"TaskCreate with subject",
			`{"type":"tool_use","name":"TaskCreate","input":{"subject":"Fix authentication bug","description":"Debug the login flow"}}`,
			"[TaskCreate: Fix authentication bug]",
		},
		{
			"TaskUpdate with status",
			`{"type":"tool_use","name":"TaskUpdate","input":{"taskId":"5","status":"completed"}}`,
			"[TaskUpdate: #5 completed]",
		},
		{
			"TaskGet",
			`{"type":"tool_use","name":"TaskGet","input":{"taskId":"3"}}`,
			"[TaskGet: #3]",
		},
		{
			"TaskList",
			`{"type":"tool_use","name":"TaskList","input":{}}`,
			"[TaskList]",
		},
		{
			"SendMessage",
			`{"type":"tool_use","name":"SendMessage","input":{"type":"message","recipient":"researcher","content":"hello"}}`,
			"[SendMessage: message to researcher]",
		},
		// Amp tools
		{
			"Read with path (Amp)",
			`{"type":"tool_use","name":"Read","input":{"path":"/tmp/foo.go"}}`,
			"[Read: /tmp/foo.go]",
		},
		{
			"Bash with cmd (Amp)",
			`{"type":"tool_use","name":"Bash","input":{"cmd":"ls -la"}}`,
			"[Bash]\n$ ls -la",
		},
		{
			"edit_file",
			`{"type":"tool_use","name":"edit_file","input":{"path":"main.go"}}`,
			"[Edit: main.go]",
		},
		{
			"create_file",
			`{"type":"tool_use","name":"create_file","input":{"path":"new.go"}}`,
			"[Write: new.go]",
		},
		{
			"shell_command",
			`{"type":"tool_use","name":"shell_command","input":{"command":"echo hi"}}`,
			"[Bash]\n$ echo hi",
		},
		{
			"glob (Amp)",
			`{"type":"tool_use","name":"glob","input":{"filePattern":"**/*.ts"}}`,
			"[Glob: **/*.ts]",
		},
		{
			"look_at",
			`{"type":"tool_use","name":"look_at","input":{"path":"diagram.png"}}`,
			"[Read: diagram.png]",
		},
		{
			"apply_patch",
			`{"type":"tool_use","name":"apply_patch","input":{"path":"fix.patch"}}`,
			"[Patch: fix.patch]",
		},
		{
			"undo_edit",
			`{"type":"tool_use","name":"undo_edit","input":{"path":"main.go"}}`,
			"[Undo: main.go]",
		},
		{
			"finder",
			`{"type":"tool_use","name":"finder","input":{"query":"JWT validation"}}`,
			"[Find: JWT validation]",
		},
		{
			"read_web_page",
			`{"type":"tool_use","name":"read_web_page","input":{"url":"https://example.com"}}`,
			"[Web: https://example.com]",
		},
		{
			"skill (Amp)",
			`{"type":"tool_use","name":"skill","input":{"name":"frontend-design"}}`,
			"[Skill: frontend-design]",
		},
		{
			json: `{"type":"tool_use","name":"empty_tool","input":{}}`,
			want: "[Tool: empty_tool]",
		},
		// Pi tools (lowercase variants)
		{
			"read (Pi)",
			`{"type":"tool_use","name":"read","input":{"file_path":"auth.go"}}`,
			"[Read: auth.go]",
		},
		{
			"read_file (Pi)",
			`{"type":"tool_use","name":"read_file","input":{"file_path":"README.md"}}`,
			"[Read: README.md]",
		},
		{
			"write (Pi)",
			`{"type":"tool_use","name":"write","input":{"file_path":"out.txt"}}`,
			"[Write: out.txt]",
		},
		{
			"edit (Pi)",
			`{"type":"tool_use","name":"edit","input":{"file_path":"config.yaml"}}`,
			"[Edit: config.yaml]",
		},
		{
			"str_replace (Pi)",
			`{"type":"tool_use","name":"str_replace","input":{"file_path":"server.go"}}`,
			"[Edit: server.go]",
		},
		{
			"bash (Pi)",
			`{"type":"tool_use","name":"bash","input":{"command":"ls -la"}}`,
			"[Bash]\n$ ls -la",
		},
		{
			"run_command (Pi)",
			`{"type":"tool_use","name":"run_command","input":{"command":"go test"}}`,
			"[Bash]\n$ go test",
		},
		{
			"find (Pi)",
			`{"type":"tool_use","name":"find","input":{"pattern":"*.go"}}`,
			"[Find: *.go]",
		},
		// Pi path fallback variants
		{
			"read with path fallback (Pi)",
			`{"type":"tool_use","name":"read","input":{"path":"src/main.go"}}`,
			"[Read: src/main.go]",
		},
		{
			"write with path fallback (Pi)",
			`{"type":"tool_use","name":"write","input":{"path":"out.txt"}}`,
			"[Write: out.txt]",
		},
		{
			"edit with filePath fallback (Pi)",
			`{"type":"tool_use","name":"edit","input":{"filePath":"app.ts"}}`,
			"[Edit: app.ts]",
		},
		{
			"str_replace with path fallback (Pi)",
			`{"type":"tool_use","name":"str_replace","input":{"path":"server.go"}}`,
			"[Edit: server.go]",
		},
		{
			"read_file with path fallback (Pi)",
			`{"type":"tool_use","name":"read_file","input":{"path":"README.md"}}`,
			"[Read: README.md]",
		},
	}

	for _, tt := range tests {
		testName := tt.toolName
		if testName == "" {
			testName = "empty_string"
		}
		t.Run(testName, func(t *testing.T) {
			block := gjson.Parse(tt.json)
			got := formatToolUse(block)
			assert.Equal(t, tt.want, got, "formatToolUse")
		})
	}
}

func TestParseTimestamp(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantUTC time.Time
		wantOK  bool
	}{
		{
			"empty string",
			"",
			time.Time{},
			false,
		},
		{
			"RFC3339 UTC",
			"2024-01-15T10:30:00Z",
			testJan15_1030UTC,
			true,
		},
		{
			"RFC3339Nano UTC",
			"2024-01-15T10:30:00.123456789Z",
			time.Date(
				2024, 1, 15, 10, 30, 0, 123456789,
				time.UTC,
			),
			true,
		},
		{
			"milliseconds with Z",
			"2024-01-15T10:30:00.500Z",
			time.Date(
				2024, 1, 15, 10, 30, 0, 500000000,
				time.UTC,
			),
			true,
		},
		{
			"positive timezone offset",
			"2024-01-15T15:30:00+05:00",
			testJan15_1030UTC,
			true,
		},
		{
			"negative timezone offset",
			"2024-01-15T03:30:00-07:00",
			testJan15_1030UTC,
			true,
		},
		{
			"millis with offset",
			"2024-01-15T15:30:00.500+05:00",
			time.Date(
				2024, 1, 15, 10, 30, 0, 500000000,
				time.UTC,
			),
			true,
		},
		{
			"space-separated datetime",
			"2024-01-15 10:30:00",
			testJan15_1030UTC,
			true,
		},
		{
			"unparseable value",
			"not-a-timestamp",
			time.Time{},
			false,
		},
		{
			"date only",
			"2024-01-15",
			time.Time{},
			false,
		},
		{
			"unix epoch number string",
			"1705315800",
			time.Time{},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTimestamp(tt.input)
			if tt.wantOK {
				require.Falsef(t, got.IsZero(),
					"parseTimestamp(%q) = zero, want %v", tt.input, tt.wantUTC)
				assert.Truef(t, got.Equal(tt.wantUTC),
					"parseTimestamp(%q) = %v, want %v", tt.input, got, tt.wantUTC)
				assert.Equalf(t, time.UTC, got.Location(),
					"parseTimestamp(%q) location", tt.input)
			} else {
				assert.Truef(t, got.IsZero(),
					"parseTimestamp(%q) = %v, want zero", tt.input, got)
			}
		})
	}
}

func TestClaudeSessionTimestampSemantics(t *testing.T) {
	t.Run("snapshot.timestamp fallback", func(t *testing.T) {
		content := testjsonl.ClaudeSnapshotJSON("2024-06-15T12:00:00Z")
		sess, msgs := parseClaudeTestFile(
			t, "ts-fallback.jsonl", content, "proj",
		)
		wantTS := time.Date(
			2024, 6, 15, 12, 0, 0, 0, time.UTC,
		)
		assertTimestamp(t, sess.StartedAt, wantTS)
		assertTimestamp(t, sess.EndedAt, wantTS)

		require.Len(t, msgs, 1, "messages")
		assertTimestamp(t, msgs[0].Timestamp, wantTS)
	})

	t.Run("offset timestamp normalized to UTC", func(t *testing.T) {
		content := testjsonl.ClaudeUserJSON("hello", "2024-06-15T17:00:00+05:00")
		sess, msgs := parseClaudeTestFile(
			t, "ts-offset.jsonl", content, "proj",
		)
		wantUTC := time.Date(
			2024, 6, 15, 12, 0, 0, 0, time.UTC,
		)
		assertTimestamp(t, sess.StartedAt, wantUTC)

		require.Len(t, msgs, 1, "messages")
		assertTimestamp(t, msgs[0].Timestamp, wantUTC)
	})

	t.Run("unparseable timestamp yields zero", func(t *testing.T) {
		content := testjsonl.ClaudeUserJSON("hello", "garbage")
		sess, msgs := parseClaudeTestFile(
			t, "ts-bad.jsonl", content, "proj",
		)
		assertZeroTimestamp(t, sess.StartedAt, "StartedAt")
		require.Len(t, msgs, 1, "messages")
		assertZeroTimestamp(t, msgs[0].Timestamp, "msg timestamp")
	})

	t.Run("invalid primary but valid fallback logs no warning", func(t *testing.T) {
		content := `{"type":"user","timestamp":"garbage","snapshot":{"timestamp":"2024-06-15T12:00:00Z"},"message":{"content":"hello"}}` + "\n"
		buf := captureLog(t)

		sess, msgs := parseClaudeTestFile(
			t, "ts-mixed.jsonl", content, "proj",
		)

		wantTS := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
		assertTimestamp(t, sess.StartedAt, wantTS)
		require.Len(t, msgs, 1, "messages")
		assertTimestamp(t, msgs[0].Timestamp, wantTS)
		assertLogEmpty(t, buf)
	})

	t.Run("both timestamps invalid logs warning", func(t *testing.T) {
		content := `{"type":"user","timestamp":"garbage1","snapshot":{"timestamp":"garbage2"},"message":{"content":"hello"}}` + "\n"
		buf := captureLog(t)

		sess, _ := parseClaudeTestFile(
			t, "ts-invalid-both.jsonl", content, "proj",
		)

		assertZeroTimestamp(t, sess.StartedAt, "StartedAt")
		assertLogContains(t, buf,
			"unparseable timestamp", "garbage1",
		)
	})

	t.Run("very long invalid timestamp is truncated in log", func(t *testing.T) {
		longInvalid := strings.Repeat("x", 200)
		content := `{"type":"user","timestamp":"` + longInvalid + `","message":{"content":"hello"}}` + "\n"
		buf := captureLog(t)

		path := createTestFile(t, "ts-long-invalid.jsonl", content)
		_, err := ParseClaudeSession(
			path, "proj", "local",
		)
		require.NoError(t, err, "ParseClaudeSession")

		assertLogContains(t, buf,
			"unparseable timestamp", "x...",
		)
		assert.LessOrEqualf(t, buf.Len(), 1000,
			"log output too long: %d bytes", buf.Len())
		assertLogNotContains(t, buf, longInvalid)
	})
}

func createTestFile(
	t *testing.T, name, content string,
) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoErrorf(t, os.WriteFile(path, []byte(content), 0o644), "create %s", name)
	return path
}

func TestIsClaudeSystemMessage(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"context continuation",
			"This session is being continued from a previous conversation.",
			true},
		{"request interrupted",
			"[Request interrupted by user]", true},
		{"task-notification",
			"<task-notification>some data</task-notification>",
			true},
		{"command-message is not system",
			"<command-message>foo</command-message>", false},
		{"command-name is not system",
			"<command-name>commit</command-name>", false},
		{"command-message with args is not system",
			"<command-message>roborev-fix</command-message>\n<command-name>/roborev-fix</command-name>\n<command-args>450</command-args>",
			false},
		{"local-command tag",
			"<local-command-result>ok</local-command-result>",
			true},
		{"stop hook feedback",
			"Stop hook feedback: rejected by policy", true},
		{"leading whitespace trimmed",
			"  \n This session is being continued...",
			true},
		{"leading tabs trimmed",
			"\t<task-notification>data</task-notification>",
			true},
		{"BOM prefix trimmed",
			"\uFEFFThis session is being continued...",
			true},
		{"BOM plus whitespace trimmed",
			"\uFEFF \t<task-notification>data</task-notification>",
			true},
		{"whitespace before BOM trimmed",
			" \uFEFFThis session is being continued...",
			true},
		{"normal user message",
			"Fix the login bug", false},
		{"implement plan is not filtered",
			"Implement the following plan:\n## Steps",
			false},
		{"empty string", "", false},
		{"partial prefix mismatch",
			"This session was great", false},
		{"assistant-like content not matched",
			"Looking at the auth module...", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isClaudeSystemMessage(tt.content)
			assert.Equalf(t, tt.want, got,
				"isClaudeSystemMessage(%q)", tt.content)
		})
	}
}

func TestExtractCommandText(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		ok      bool
	}{
		{
			"skill with args",
			"<command-message>roborev-fix</command-message>\n<command-name>/roborev-fix</command-name>\n<command-args>450</command-args>",
			"/roborev-fix 450",
			true,
		},
		{
			"skill without args",
			"<command-message>superpowers:brainstorming</command-message>\n<command-name>/superpowers:brainstorming</command-name>",
			"/superpowers:brainstorming",
			true,
		},
		{
			"command-name first format",
			"<command-name>/model</command-name>\n            <command-message>model</command-message>\n            <command-args></command-args>",
			"/model",
			true,
		},
		{
			"command-name first with args",
			"<command-name>/roborev-fix</command-name>\n<command-message>roborev-fix</command-message>\n<command-args>491</command-args>",
			"/roborev-fix 491",
			true,
		},
		{
			"not a command message",
			"Fix the login bug",
			"",
			false,
		},
		{
			"empty string",
			"",
			"",
			false,
		},
		{
			"prose mentioning command-name tag not rewritten",
			"The <command-name>foo</command-name> tag is used for...",
			"",
			false,
		},
		{
			"tag followed by prose not rewritten",
			"<command-message>foo</command-message> is the XML wrapper...",
			"",
			false,
		},
		{
			"bare command-message without command-name",
			"<command-message>insights</command-message>",
			"/insights",
			true,
		},
		{
			"BOM-prefixed command envelope",
			"\uFEFF<command-name>/commit</command-name>\n<command-message>commit</command-message>",
			"/commit",
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractCommandText(tt.content)
			assert.Equalf(t, tt.ok, ok,
				"extractCommandText(%q) ok", tt.content)
			assert.Equalf(t, tt.want, got,
				"extractCommandText(%q)", tt.content)
		})
	}
}

func TestCodexUserMessageCount(t *testing.T) {
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"umc-test", "/Users/alice/code/app", "user", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "Fix the tests", tsEarlyS1),
		testjsonl.CodexFunctionCallJSON(
			"shell_command", "Running tests", tsEarlyS5,
		),
		testjsonl.CodexMsgJSON("assistant", "Tests pass now", tsLate),
		testjsonl.CodexMsgJSON("user", "Great, thanks", tsLateS5),
	)

	path := createTestFile(t, "codex-umc.jsonl", content)
	sess, msgs, err := ParseCodexSession(path, "local", false)
	require.NoError(t, err, "ParseCodexSession")
	require.NotNil(t, sess, "session")
	require.Len(t, msgs, 4, "messages")
	// 2 user messages with real text content.
	assert.Equal(t, 2, sess.UserMessageCount, "UserMessageCount")
}

func TestCodexSessionTimestampSemantics(t *testing.T) {
	t.Run("invalid timestamp logs warning", func(t *testing.T) {
		content := testjsonl.CodexMsgJSON("user", "hello", "garbage") + "\n"
		path := createTestFile(t, "codex-ts-invalid.jsonl", content)
		buf := captureLog(t)

		sess, msgs, err := ParseCodexSession(
			path, "local", false,
		)
		require.NoError(t, err, "ParseCodexSession")

		assertZeroTimestamp(t, sess.StartedAt, "StartedAt")
		require.Len(t, msgs, 1, "messages")
		assertZeroTimestamp(t, msgs[0].Timestamp, "msg timestamp")
		assertLogContains(t, buf,
			"unparseable timestamp", "garbage",
		)
	})

	t.Run("very long invalid timestamp is truncated in log", func(t *testing.T) {
		longInvalid := strings.Repeat("x", 200)
		content := testjsonl.CodexMsgJSON("user", "hello", longInvalid) + "\n"
		path := createTestFile(t, "codex-ts-long-invalid.jsonl", content)
		buf := captureLog(t)

		_, _, err := ParseCodexSession(
			path, "local", false,
		)
		require.NoError(t, err, "ParseCodexSession")

		assertLogContains(t, buf,
			"unparseable timestamp", "...",
		)
		assertLogNotContains(t, buf, longInvalid)
	})
}

func TestParseCodexSessionOversizedLineSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates large buffer")
	}

	meta := testjsonl.CodexSessionMetaJSON(
		"huge", "/tmp", "user", tsEarly,
	) + "\n"
	prefix := `{"type":"response_item","timestamp":"` +
		tsEarlyS1 +
		`","payload":{"role":"user","content":` +
		`[{"type":"input_text","text":"`
	suffix := `"}]}}` + "\n"

	normalLine := prefix + "hello" + suffix
	oversizedLine := prefix +
		strings.Repeat("x", maxLineSize+1) + suffix

	// Place the oversized line between two normal lines.
	content := meta + normalLine + oversizedLine + normalLine
	path := createTestFile(t, "oversized.jsonl", content)
	sess, msgs, err := ParseCodexSession(
		path, "local", false,
	)
	require.NoError(t, err, "unexpected error")
	require.NotNil(t, sess, "session")
	require.Len(t, msgs, 2, "messages (oversized skipped)")
}

func TestExtractCwdFromSession(t *testing.T) {
	tests := []struct {
		name    string
		content string // empty means use nonexistent file
		want    string
	}{
		{
			"has cwd field",
			`{"type":"user","timestamp":"2024-01-01T00:00:00Z","message":{"content":"hi"},"cwd":"/Users/alice/code/my-app"}` + "\n",
			"/Users/alice/code/my-app",
		},
		{
			"no cwd field",
			`{"type":"user","timestamp":"2024-01-01T00:00:00Z","message":{"content":"hi"}}` + "\n",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := createTestFile(t, "test.jsonl", tt.content)
			got := ExtractCwdFromSession(path)
			assert.Equal(t, tt.want, got, "ExtractCwdFromSession")
		})
	}

	t.Run("missing file", func(t *testing.T) {
		got := ExtractCwdFromSession("/nonexistent/path.jsonl")
		assert.Empty(t, got, "ExtractCwdFromSession")
	})
}

func TestParseCodexSession_WorktreeBranchFallback(t *testing.T) {
	content := `{"type":"session_meta","timestamp":"2024-01-01T00:00:00Z","payload":{"id":"test-uuid","cwd":"/Users/wesm/code/agentsview-worktree-tool-call-arguments","originator":"user","git":{"branch":"worktree-tool-call-arguments"}}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T00:00:01Z","payload":{"role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n"
	path := createTestFile(t, "codex-worktree.jsonl", content)

	sess, _, err := ParseCodexSession(path, "local", false)
	require.NoError(t, err, "ParseCodexSession")
	require.NotNil(t, sess, "session")
	assert.Equal(t, "agentsview", sess.Project, "project")
}

func TestExtractClaudeProjectHints(t *testing.T) {
	t.Run("extracts cwd and gitBranch", func(t *testing.T) {
		content := `{"type":"user","timestamp":"2024-01-01T00:00:00Z","cwd":"/Users/alice/code/my-app-worktree-fix","gitBranch":"worktree-fix","message":{"content":"hi"}}` + "\n"
		path := createTestFile(t, "hints.jsonl", content)

		cwd, branch := ExtractClaudeProjectHints(path)
		require.Equal(t, "/Users/alice/code/my-app-worktree-fix", cwd, "cwd")
		require.Equal(t, "worktree-fix", branch, "branch")
	})

	t.Run("missing branch still returns cwd", func(t *testing.T) {
		content := `{"type":"user","timestamp":"2024-01-01T00:00:00Z","cwd":"/Users/alice/code/my-app","message":{"content":"hi"}}` + "\n"
		path := createTestFile(t, "hints-nobranch.jsonl", content)

		cwd, branch := ExtractClaudeProjectHints(path)
		require.Equal(t, "/Users/alice/code/my-app", cwd, "cwd")
		require.Empty(t, branch, "branch")
	})

	t.Run("missing file", func(t *testing.T) {
		cwd, branch := ExtractClaudeProjectHints(
			"/nonexistent/path.jsonl",
		)
		require.Empty(t, cwd, "cwd")
		require.Empty(t, branch, "branch")
	})
}

func TestFormatGeminiToolCall(t *testing.T) {
	tests := []struct {
		toolName string
		json     string
		want     string
	}{
		{
			"read_file",
			`{"name":"read_file","args":{"file_path":"main.go"},"displayName":"ReadFile"}`,
			"[Read: main.go]",
		},
		{
			"write_file",
			`{"name":"write_file","args":{"file_path":"out.txt"},"displayName":"WriteFile"}`,
			"[Write: out.txt]",
		},
		{
			"edit_file",
			`{"name":"edit_file","args":{"file_path":"fix.go"},"displayName":"EditFile"}`,
			"[Edit: fix.go]",
		},
		{
			"replace",
			`{"name":"replace","args":{"file_path":"fix.go"},"displayName":"Replace"}`,
			"[Edit: fix.go]",
		},
		{
			"run_command",
			`{"name":"run_command","args":{"command":"go test ./..."},"displayName":"RunCommand"}`,
			"[Bash]\n$ go test ./...",
		},
		{
			"execute_command",
			`{"name":"execute_command","args":{"command":"ls -la"},"displayName":"Exec"}`,
			"[Bash]\n$ ls -la",
		},
		{
			"run_shell_command",
			`{"name":"run_shell_command","args":{"command":"make build"},"displayName":"Shell"}`,
			"[Bash]\n$ make build",
		},
		{
			"list_directory",
			`{"name":"list_directory","args":{"dir_path":"src"},"displayName":"ReadFolder"}`,
			"[List: src]",
		},
		{
			"search_files with query",
			`{"name":"search_files","args":{"query":"TODO"},"displayName":"Search"}`,
			"[Grep: TODO]",
		},
		{
			"grep with pattern",
			`{"name":"grep","args":{"pattern":"func main"},"displayName":"Grep"}`,
			"[Grep: func main]",
		},
		{
			"grep_search with query",
			`{"name":"grep_search","args":{"query":"TODO"},"displayName":"GrepSearch"}`,
			"[Grep: TODO]",
		},
		{
			"glob",
			`{"name":"glob","args":{"pattern":"**/*.go"},"displayName":"Glob"}`,
			"[Glob: **/*.go]",
		},
		{
			"unknown tool with displayName",
			`{"name":"custom_tool","args":{},"displayName":"CustomTool"}`,
			"[Tool: CustomTool]",
		},
		{
			"unknown tool without displayName",
			`{"name":"custom_tool","args":{}}`,
			"[Tool: custom_tool]",
		},
		{
			"",
			`{"name":"empty_tool","args":{}}`,
			"[Tool: empty_tool]",
		},
	}

	for _, tt := range tests {
		testName := tt.toolName
		if testName == "" {
			testName = "empty_string"
		}
		t.Run(testName, func(t *testing.T) {
			tc := gjson.Parse(tt.json)
			got := formatGeminiToolCall(tc)
			assert.Equal(t, tt.want, got, "formatGeminiToolCall")
		})
	}
}

func TestGeminiUserMessageCount(t *testing.T) {
	hash := "abc123def456"
	content := testjsonl.GeminiSessionJSON(
		"umc-gemini", hash, tsEarly, tsLateS5,
		[]map[string]any{
			testjsonl.GeminiUserMsg("u1", tsEarly, "Fix the bug"),
			testjsonl.GeminiAssistantMsg(
				"a1", tsEarlyS5, "Looking at it.", nil,
			),
			testjsonl.GeminiUserMsg("u2", tsLate, "Ship it"),
			testjsonl.GeminiAssistantMsg(
				"a2", tsLateS5, "Done.", nil,
			),
		},
	)

	path := createTestFile(t, "gemini-umc.json", content)
	sess, msgs, err := ParseGeminiSession(
		path, "my_project", "local",
	)
	require.NoError(t, err, "ParseGeminiSession")
	require.NotNil(t, sess, "session")
	require.Len(t, msgs, 4, "messages")
	assert.Equal(t, 2, sess.UserMessageCount, "UserMessageCount")
}

func TestGeminiSessionID(t *testing.T) {
	data := []byte(`{"sessionId":"abc-123","messages":[]}`)
	got := GeminiSessionID(data)
	assert.Equal(t, "abc-123", got, "GeminiSessionID")

	got = GeminiSessionID([]byte("{\"sessionId\":\"jsonl-123\"}\n{\"type\":\"user\"}\n"))
	assert.Equal(t, "jsonl-123", got, "GeminiSessionID JSONL")

	got = GeminiSessionID([]byte(`{}`))
	assert.Empty(t, got, "GeminiSessionID empty")
}

func TestClaudeUserMessageCount(t *testing.T) {
	tests := []struct {
		name          string
		content       string
		wantUserCount int
		wantMsgCount  int
	}{
		{
			name: "counts real user prompts only",
			content: testjsonl.JoinJSONL(
				testjsonl.ClaudeUserJSON("Fix the bug", tsEarly),
				testjsonl.ClaudeAssistantJSON([]map[string]any{
					{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]string{"file_path": "main.go"}},
				}, tsEarlyS1),
				// Tool-result user message: Content="" but has tool_result blocks.
				// This should NOT count as a user prompt.
				`{"type":"user","timestamp":"`+tsEarlyS5+`","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"package main"}]}}`,
				testjsonl.ClaudeAssistantJSON([]map[string]any{
					{"type": "text", "text": "Here is the fix."},
				}, tsLate),
				testjsonl.ClaudeUserJSON("Thanks!", tsLateS5),
			),
			wantUserCount: 2,
			wantMsgCount:  5,
		},
		{
			name: "no user prompts in tool-only session",
			content: testjsonl.JoinJSONL(
				testjsonl.ClaudeAssistantJSON([]map[string]any{
					{"type": "tool_use", "id": "toolu_2", "name": "Bash", "input": map[string]string{"command": "ls"}},
				}, tsEarly),
				`{"type":"user","timestamp":"`+tsEarlyS1+`","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"file.go"}]}}`,
			),
			wantUserCount: 0,
			wantMsgCount:  2,
		},
		{
			name: "single user prompt",
			content: testjsonl.JoinJSONL(
				testjsonl.ClaudeUserJSON("Hello", tsEarly),
				testjsonl.ClaudeAssistantJSON([]map[string]any{
					{"type": "text", "text": "Hi!"},
				}, tsEarlyS5),
			),
			wantUserCount: 1,
			wantMsgCount:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := createTestFile(t, "test.jsonl", tt.content)
			results, err := ParseClaudeSession(
				path, "test-proj", "local",
			)
			require.NoError(t, err, "ParseClaudeSession")
			require.NotEmpty(t, results, "ParseClaudeSession returned no results")
			sess := results[0].Session
			msgs := results[0].Messages
			require.Len(t, msgs, tt.wantMsgCount, "message count")
			assert.Equal(t, tt.wantUserCount, sess.UserMessageCount, "UserMessageCount")
		})
	}
}

func TestParseClaudeToolResults(t *testing.T) {
	lines := []string{
		`{"type":"assistant","timestamp":"2024-01-01T00:00:00Z","message":{"content":[{"type":"tool_use","id":"toolu_abc","name":"Read","input":{"file_path":"main.go"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_abc","content":"package main\nfunc main() {}"}]}}`,
	}
	content := strings.Join(lines, "\n") + "\n"
	path := createTestFile(t, "tool-results.jsonl", content)

	results, err := ParseClaudeSession(path, "test-project", "local")
	require.NoError(t, err, "ParseClaudeSession")
	require.NotEmpty(t, results, "ParseClaudeSession returned no results")
	msgs := results[0].Messages

	// Should have 2 messages: assistant tool_use + user tool_result
	require.Len(t, msgs, 2, "messages")

	// User message should have ToolResults populated
	userMsg := msgs[1]
	require.Len(t, userMsg.ToolResults, 1, "ToolResults count")
	assert.Equal(t, "toolu_abc", userMsg.ToolResults[0].ToolUseID, "ToolUseID")
	assert.Equal(t, 27, userMsg.ToolResults[0].ContentLength, "ContentLength")
}
