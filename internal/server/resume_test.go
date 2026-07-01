package server

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func canonicalTestDir(path string) string {
	if path == "" {
		return ""
	}
	if normalized := normalizeCursorDir(path); normalized != "" {
		return normalized
	}
	return filepath.Clean(path)
}

func assertSameDir(t *testing.T, label, got, want string) {
	t.Helper()
	got = canonicalTestDir(got)
	want = canonicalTestDir(want)
	assert.Equal(t, want, got, label)
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple uuid", "abc-123-def", "abc-123-def"},
		{"alphanumeric", "session42", "session42"},
		{"with colon", "a:b", "'a:b'"},
		{"with spaces", "has space", "'has space'"},
		{"with single quote", "it's", `'it'"'"'s'`},
		{"command injection attempt", "$(whoami)", "'$(whoami)'"},
		{"backtick injection", "`rm -rf /`", "'`rm -rf /`'"},
		{"semicolon", "id;rm -rf /", "'id;rm -rf /'"},
		{"pipe", "id|cat", "'id|cat'"},
		{"empty passthrough", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellQuote(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCommandWithCleanup(t *testing.T) {
	assert.Equal(t,
		"claude < prompt.txt; rm -f -- 'prompt.txt'",
		commandWithCleanup("claude < prompt.txt", "prompt.txt"),
	)
	assert.Equal(t,
		"claude < prompt.txt",
		commandWithCleanup("claude < prompt.txt", ""),
	)
}

func TestClaudeMessagePointResponseCommandUsesPOSIXShellOffWindows(
	t *testing.T,
) {
	cwd := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "prompt file.txt")

	got := claudeMessagePointResponseCommand(promptPath, true, cwd, "linux")

	want := "cd " + shellQuote(cwd) + " && claude --dangerously-skip-permissions < " +
		shellQuote(promptPath) + "; rm -f -- " + shellQuote(promptPath)
	assert.Equal(t, want, got)
}

func TestClaudeMessagePointResponseCommandUsesPowerShellOnWindows(
	t *testing.T,
) {
	promptPath := `C:\Users\Ada\AppData\Local\Temp\prompt's file.txt`
	cwd := `C:\Users\Ada\source\agentsview`

	got := claudeMessagePointResponseCommand(
		promptPath, true, cwd, "windows",
	)

	const prefix = "powershell.exe -NoProfile -EncodedCommand "
	require.True(t, strings.HasPrefix(got, prefix), "command = %q", got)
	assert.NotContains(t, got, " < ")
	assert.NotContains(t, got, "rm -f --")

	script := decodePowerShellEncodedCommandForTest(
		t, strings.TrimPrefix(got, prefix),
	)
	assert.Contains(t, script,
		"Set-Location -LiteralPath 'C:\\Users\\Ada\\source\\agentsview'")
	assert.Contains(t, script,
		"Get-Content -Raw -Encoding UTF8 -LiteralPath "+
			"'C:\\Users\\Ada\\AppData\\Local\\Temp\\prompt''s file.txt' | "+
			"& 'claude' --dangerously-skip-permissions")
	assert.Contains(t, script,
		"Remove-Item -LiteralPath "+
			"'C:\\Users\\Ada\\AppData\\Local\\Temp\\prompt''s file.txt' "+
			"-Force -ErrorAction SilentlyContinue")
	assert.NotContains(t, script, " < ")
	assert.NotContains(t, script, "rm -f --")
}

func decodePowerShellEncodedCommandForTest(
	t *testing.T, encoded string,
) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	require.Zero(t, len(raw)%2, "UTF-16LE byte length must be even")

	codeUnits := make([]uint16, len(raw)/2)
	for i := range codeUnits {
		codeUnits[i] = binary.LittleEndian.Uint16(raw[i*2:])
	}
	return string(utf16.Decode(codeUnits))
}

func TestClaudeMessagePointPromptPathIsUniquePerLaunch(t *testing.T) {
	first, err := claudeMessagePointPromptPath("sess-1", 2)
	require.NoError(t, err)
	second, err := claudeMessagePointPromptPath("sess-1", 2)
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
}

func TestDetectTerminalLinux_NoTerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only terminal detection")
	}
	// Empty PATH and no $TERMINAL — no terminal should be found.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("TERMINAL", "")
	_, _, _, err := detectTerminalLinux("echo test")
	assert.Error(t, err, "expected error with empty PATH")
}

func TestDetectTerminalLinux_EnvTerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only terminal detection")
	}
	// Create a fake terminal binary on PATH.
	binDir := t.TempDir()
	fakeBin := filepath.Join(binDir, "myterm")
	require.NoError(t,
		os.WriteFile(fakeBin, []byte("#!/bin/sh\n"), 0o755),
	)
	t.Setenv("PATH", binDir)
	t.Setenv("TERMINAL", "myterm")

	bin, args, name, err := detectTerminalLinux("echo hello")
	require.NoError(t, err)
	assert.Equal(t, fakeBin, bin)
	assert.Equal(t, "myterm", name)
	assert.NotEmpty(t, args, "expected non-empty args")
}

func TestDetectTerminalLinux_EnvTerminalWithArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only terminal detection")
	}
	binDir := t.TempDir()
	fakeBin := filepath.Join(binDir, "kitty")
	require.NoError(t,
		os.WriteFile(fakeBin, []byte("#!/bin/sh\n"), 0o755),
	)
	t.Setenv("PATH", binDir)
	t.Setenv("TERMINAL", "kitty --single-instance")

	bin, args, name, err := detectTerminalLinux("echo hello")
	require.NoError(t, err)
	assert.Equal(t, fakeBin, bin)
	assert.Equal(t, "kitty", name)
	// Should have --single-instance prepended before template args.
	require.GreaterOrEqual(t, len(args), 2,
		"args = %v, want --single-instance as first arg", args)
	assert.Equal(t, "--single-instance", args[0],
		"args = %v, want --single-instance as first arg", args)
}

func TestLaunchClaudeDesktop(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		cwd       string
		wantArg   string
	}{
		{
			name:      "simple session id",
			sessionID: "abc-123",
			cwd:       "",
			wantArg:   "claude://resume?session=abc-123",
		},
		{
			name:      "session id with cwd",
			sessionID: "abc-123",
			cwd:       "/Users/test/project",
			wantArg:   "claude://resume?session=abc-123&cwd=%2FUsers%2Ftest%2Fproject",
		},
		{
			name:      "cwd with spaces",
			sessionID: "sess-1",
			cwd:       "/Users/test/my project",
			wantArg:   "claude://resume?session=sess-1&cwd=%2FUsers%2Ftest%2Fmy+project",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := launchClaudeDesktop(tt.sessionID, tt.cwd)
			require.NotEmpty(t, cmd.Path,
				"expected non-empty command path")
			// The command should be "open <url>".
			args := cmd.Args
			require.Len(t, args, 2,
				"args = %v, want 2 elements", args)
			assert.Equal(t, tt.wantArg, args[1])
		})
	}
}

func TestReadSessionCwd_LargeLine(t *testing.T) {
	// Verify that readSessionCwd handles lines larger than the
	// old 2MB scanner limit without losing the cwd field.
	dir := t.TempDir()
	cwdDir := filepath.Join(dir, "project")
	require.NoError(t, os.Mkdir(cwdDir, 0o755))

	cwdJSON, _ := json.Marshal(cwdDir)
	// Build a 3MB padding string to exceed the old scanner limit.
	padding := strings.Repeat("x", 3*1024*1024)
	line := `{"cwd":` + string(cwdJSON) +
		`,"big":"` + padding + `"}` + "\n"

	sessionFile := filepath.Join(dir, "session.jsonl")
	require.NoError(t, os.WriteFile(sessionFile, []byte(line), 0o644))

	got := readSessionCwd(sessionFile)
	assert.Equal(t, cwdDir, got)
}

func TestReadSessionCwd_CopilotFormat(t *testing.T) {
	dir := t.TempDir()
	cwdDir := filepath.Join(dir, "project")
	require.NoError(t, os.Mkdir(cwdDir, 0o755))

	cwdJSON, _ := json.Marshal(cwdDir)
	line := `{"type":"session.start","data":{"sessionId":"abc","context":{"cwd":` +
		string(cwdJSON) + `}}}` + "\n"

	sessionFile := filepath.Join(dir, "session.jsonl")
	require.NoError(t, os.WriteFile(sessionFile, []byte(line), 0o644))

	got := readSessionCwd(sessionFile)
	assert.Equal(t, cwdDir, got)
}

func TestReadCursorLastWorkingDir(t *testing.T) {
	dir := t.TempDir()
	workspaceDir := filepath.Join(dir, "project")
	lastDir := filepath.Join(workspaceDir, "frontend")
	require.NoError(t, os.MkdirAll(lastDir, 0o755))

	firstJSON, _ := json.Marshal(workspaceDir)
	lastJSON, _ := json.Marshal(lastDir)
	sessionFile := filepath.Join(dir, "cursor.jsonl")
	content := "" +
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"ReadFile","input":{"path":"/tmp/file.txt"}}]}}` + "\n" +
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"pwd","working_directory":` + string(firstJSON) + `}}]}}` + "\n" +
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"pwd","working_directory":"relative/path"}}]}}` + "\n" +
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"pwd","working_directory":` + string(lastJSON) + `}}]}}` + "\n"
	require.NoError(t, os.WriteFile(sessionFile, []byte(content), 0o644))

	got := readCursorLastWorkingDir(sessionFile)
	assertSameDir(t, "readCursorLastWorkingDir()", got, lastDir)
}

func TestCursorProjectDirNameFromTranscriptPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "flat transcript",
			path: filepath.Join(
				"/tmp", ".cursor", "projects",
				"Users-alice-code-my-app",
				"agent-transcripts", "sess.jsonl",
			),
			want: "Users-alice-code-my-app",
		},
		{
			name: "nested transcript",
			path: filepath.Join(
				"/tmp", ".cursor", "projects",
				"Users-alice-code-my-app",
				"agent-transcripts", "sess", "sess.jsonl",
			),
			want: "Users-alice-code-my-app",
		},
		{
			name: "missing agent transcripts ancestor",
			path: filepath.Join(
				"/tmp", ".cursor", "projects",
				"Users-alice-code-my-app", "other", "sess.jsonl",
			),
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cursorProjectDirNameFromTranscriptPath(tt.path)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveCursorProjectDirNameFromRoot(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(
		root, "Users", "alice", "code", "li",
		"project-cache-hdfs",
	)
	require.NoError(t, os.MkdirAll(want, 0o755))
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "Users", "alice", "code", "li"),
		0o755,
	))
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "Users", "alice", "code", "li", "project"),
		0o755,
	))

	got := resolveCursorProjectDirNameFromRoot(
		root, "Users-alice-code-li-project-cache-hdfs",
	)
	assert.Equal(t, want, got)
}

func TestResolveCursorProjectDirNameFromRootMatchesUnderscoreComponents(
	t *testing.T,
) {
	root := t.TempDir()
	want := filepath.Join(
		root, "Users", "alice", "code", "li",
		"project_cache_hdfs",
	)
	require.NoError(t, os.MkdirAll(want, 0o755))

	got := resolveCursorProjectDirNameFromRoot(
		root, "Users-alice-code-li-project-cache-hdfs",
	)
	assert.Equal(t, want, got)
}

func TestResolveCursorProjectDirFromSessionFileDetectsAmbiguity(
	t *testing.T,
) {
	root := t.TempDir()
	want := filepath.Join(
		root, "Users", "alice", "code", "li-tools",
	)
	require.NoError(t, os.MkdirAll(want, 0o755))
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "Users", "alice", "code", "li", "tools"),
		0o755,
	))

	filePath := filepath.Join(
		root, ".cursor", "projects",
		"Users-alice-code-li-tools",
		"agent-transcripts", "sess", "sess.jsonl",
	)
	dirName := cursorProjectDirNameFromTranscriptPath(filePath)
	matches := resolveCursorProjectDirNameFromRootMatches(
		root, dirName, "", 2,
	)
	got := ""
	if len(matches) > 0 {
		got = matches[0]
	}
	ambiguous := len(matches) > 1
	assert.Equal(t, want, got)
	assert.True(t, ambiguous, "expected ambiguous transcript path")
}

func TestResolveCursorProjectDirFromSessionFileUnambiguous(
	t *testing.T,
) {
	root := t.TempDir()
	want := filepath.Join(
		root, "Users", "alice", "code", "li-openhouse",
	)
	require.NoError(t, os.MkdirAll(want, 0o755))

	filePath := filepath.Join(
		root, ".cursor", "projects",
		"Users-alice-code-li-openhouse",
		"agent-transcripts", "sess", "sess.jsonl",
	)
	dirName := cursorProjectDirNameFromTranscriptPath(filePath)
	matches := resolveCursorProjectDirNameFromRootMatches(
		root, dirName, "", 2,
	)
	got := ""
	if len(matches) > 0 {
		got = matches[0]
	}
	ambiguous := len(matches) > 1
	assert.Equal(t, want, got)
	assert.False(t, ambiguous, "expected unambiguous transcript path")
}

func TestResolveCursorProjectDirNameFromRootBacktracksOnDeadEnd(
	t *testing.T,
) {
	root := t.TempDir()
	want := filepath.Join(
		root, "Users", "alice", "code", "li", "tools-app",
	)
	require.NoError(t, os.MkdirAll(want, 0o755))
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "Users", "alice", "code", "li-tools"),
		0o755,
	))

	got := resolveCursorProjectDirNameFromRoot(
		root, "Users-alice-code-li-tools-app",
	)
	assert.Equal(t, want, got)
}

func TestResolveCursorProjectDirNameFromRootHintPrefersContainingPath(
	t *testing.T,
) {
	root := t.TempDir()
	want := filepath.Join(
		root, "Users", "alice", "code", "li", "tools",
	)
	hint := filepath.Join(want, "frontend")
	require.NoError(t, os.MkdirAll(hint, 0o755))
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "Users", "alice", "code", "li-tools"),
		0o755,
	))

	got := resolveCursorProjectDirNameFromRootHint(
		root, "Users-alice-code-li-tools", hint,
	)
	assert.Equal(t, want, got)
}

func TestResolveCursorProjectDirNameFromRootHintStaleReturnsEmpty(
	t *testing.T,
) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "Users", "alice", "code", "li-tools"),
		0o755,
	))
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "Users", "alice", "code", "li", "tools"),
		0o755,
	))

	staleHint := filepath.Join(root, "unrelated")
	require.NoError(t, os.MkdirAll(staleHint, 0o755))

	got := resolveCursorProjectDirNameFromRootHint(
		root, "Users-alice-code-li-tools", staleHint,
	)
	assert.Empty(t, got, "with stale hint = %q, want empty", got)
}

func TestResolveCursorProjectDirNameFromRootHintSymlinkMatch(
	t *testing.T,
) {
	root := t.TempDir()

	// Real project with a hint subdir.
	realProject := filepath.Join(root, "repos", "li-tools")
	hintDir := filepath.Join(realProject, "src")
	require.NoError(t, os.MkdirAll(hintDir, 0o755))
	// Second ambiguous path.
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "repos", "li", "tools"),
		0o755,
	))

	// Symlink: root/code -> root/repos. The DFS walks through
	// the symlink but the hint uses the resolved real path.
	if err := os.Symlink(
		filepath.Join(root, "repos"),
		filepath.Join(root, "code"),
	); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	got := resolveCursorProjectDirNameFromRootHint(
		root, "code-li-tools", hintDir,
	)
	assertSameDir(t, "result", got, realProject)
}

func TestResolveSessionDir(t *testing.T) {
	// Create a real temp directory for the "absolute path" cases.
	tmpDir := t.TempDir()

	// Create a session file with a cwd field.
	sessionFile := filepath.Join(tmpDir, "session.jsonl")
	cwdDir := filepath.Join(tmpDir, "project")
	require.NoError(t, os.Mkdir(cwdDir, 0o755))
	cwdJSON, _ := json.Marshal(cwdDir)
	content := `{"cwd":` + string(cwdJSON) + `}` + "\n"
	require.NoError(t, os.WriteFile(sessionFile, []byte(content), 0o644))

	kiroStoreDir := filepath.Join(tmpDir, "kiro-store")
	require.NoError(t, os.Mkdir(kiroStoreDir, 0o755))
	kiroProjectDir := filepath.Join(tmpDir, "kiro-project")
	require.NoError(t, os.Mkdir(kiroProjectDir, 0o755))
	kiroVirtualPath := filepath.Join(kiroStoreDir, "data.sqlite3") + "#sqlite-session"

	hashPathDir := filepath.Join(tmpDir, "project#dev")
	hashPathCwd := filepath.Join(hashPathDir, "workspace")
	require.NoError(t, os.MkdirAll(hashPathCwd, 0o755))
	hashPathSessionFile := filepath.Join(hashPathDir, "session.jsonl")
	hashPathCwdJSON, _ := json.Marshal(hashPathCwd)
	hashPathContent := `{"cwd":` + string(hashPathCwdJSON) + `}` + "\n"
	require.NoError(t, os.WriteFile(
		hashPathSessionFile, []byte(hashPathContent), 0o644,
	))

	cursorProject := filepath.Join(
		tmpDir, "workspace-root", "li-openhouse",
	)
	require.NoError(t, os.MkdirAll(cursorProject, 0o755))
	cursorTranscript := filepath.Join(
		tmpDir, ".cursor", "projects",
		encodeCursorProjectPathForTest(cursorProject),
		"agent-transcripts", "cursor-sess",
		"cursor-sess.jsonl",
	)
	require.NoError(t, os.MkdirAll(
		filepath.Dir(cursorTranscript), 0o755,
	))
	require.NoError(t, os.WriteFile(cursorTranscript, []byte("{}\n"), 0o644))
	cursorLastDir := filepath.Join(cursorProject, "frontend")
	require.NoError(t, os.MkdirAll(cursorLastDir, 0o755))
	cursorLastDirJSON, _ := json.Marshal(cursorLastDir)
	cursorTranscriptWithLastDir := filepath.Join(
		tmpDir, ".cursor", "projects",
		encodeCursorProjectPathForTest(cursorProject),
		"agent-transcripts", "cursor-sess-last",
		"cursor-sess-last.jsonl",
	)
	require.NoError(t, os.MkdirAll(
		filepath.Dir(cursorTranscriptWithLastDir), 0o755,
	))
	lastDirContent := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"pwd","working_directory":` +
		string(cursorLastDirJSON) + `}}]}}` + "\n"
	require.NoError(t, os.WriteFile(
		cursorTranscriptWithLastDir, []byte(lastDirContent), 0o644,
	))

	tests := []struct {
		name    string
		session *db.Session
		want    string
	}{
		{
			name: "absolute project path",
			session: &db.Session{
				Project: tmpDir,
			},
			want: tmpDir,
		},
		{
			name: "relative project name returns empty",
			session: &db.Session{
				Project: "my-repo",
			},
			want: "",
		},
		{
			name: "nil file_path with relative project",
			session: &db.Session{
				Project:  "my-repo",
				FilePath: nil,
			},
			want: "",
		},
		{
			name: "file_path with cwd in session file",
			session: &db.Session{
				Project:  "my-repo",
				FilePath: &sessionFile,
			},
			want: cwdDir,
		},
		{
			name: "file_path takes precedence over project",
			session: &db.Session{
				Project:  tmpDir,
				FilePath: &sessionFile,
			},
			want: cwdDir,
		},
		{
			name: "file_path takes precedence over cached cwd",
			session: &db.Session{
				Cwd:      tmpDir,
				FilePath: &sessionFile,
			},
			want: cwdDir,
		},
		{
			name: "nonexistent file_path falls back to project",
			session: func() *db.Session {
				bad := "/nonexistent/session.jsonl"
				return &db.Session{
					Project:  tmpDir,
					FilePath: &bad,
				}
			}(),
			want: tmpDir,
		},
		{
			name: "kiro sqlite virtual path uses cached cwd not storage dir",
			session: &db.Session{
				Agent:    "kiro",
				Cwd:      kiroProjectDir,
				Project:  kiroStoreDir,
				FilePath: &kiroVirtualPath,
			},
			want: kiroProjectDir,
		},
		{
			name: "real file path with hash still reads embedded cwd",
			session: &db.Session{
				Cwd:      tmpDir,
				FilePath: &hashPathSessionFile,
			},
			want: hashPathCwd,
		},
		{
			name: "cursor transcript path resolves workspace dir",
			session: &db.Session{
				Agent:    "cursor",
				Project:  cursorProject,
				FilePath: &cursorTranscript,
			},
			want: cursorProject,
		},
		{
			name: "cursor transcript with last shell dir still resolves workspace",
			session: &db.Session{
				Agent:    "cursor",
				Project:  cursorProject,
				FilePath: &cursorTranscriptWithLastDir,
			},
			want: cursorProject,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveSessionDir(tt.session)
			if tt.want == "" {
				assert.Empty(t, got,
					"resolveSessionDir() = %q, want empty", got)
				return
			}
			assert.Equal(t,
				canonicalTestDir(tt.want),
				canonicalTestDir(got),
				"resolveSessionDir()",
			)
		})
	}
}

func TestResolveResumeDir(t *testing.T) {
	tmpDir := t.TempDir()

	cursorProject := filepath.Join(
		tmpDir, "workspace-root", "li-openhouse",
	)
	cursorLastDir := filepath.Join(cursorProject, "frontend")
	require.NoError(t, os.MkdirAll(cursorLastDir, 0o755))

	cursorLastDirJSON, _ := json.Marshal(cursorLastDir)
	cursorTranscript := filepath.Join(
		tmpDir, ".cursor", "projects",
		encodeCursorProjectPathForTest(cursorProject),
		"agent-transcripts", "cursor-sess-last",
		"cursor-sess-last.jsonl",
	)
	require.NoError(t, os.MkdirAll(
		filepath.Dir(cursorTranscript), 0o755,
	))
	lastDirContent := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"pwd","working_directory":` +
		string(cursorLastDirJSON) + `}}]}}` + "\n"
	require.NoError(t, os.WriteFile(
		cursorTranscript, []byte(lastDirContent), 0o644,
	))

	got := resolveResumeDir(&db.Session{
		Agent:    "cursor",
		Project:  "li_openhouse",
		FilePath: &cursorTranscript,
	})
	assertSameDir(t, "resolveResumeDir()", got, cursorLastDir)
}

func TestResolveCursorWorkspaceDirUsesLastWorkingDirHint(t *testing.T) {
	tmpDir := t.TempDir()

	cursorProject := filepath.Join(
		tmpDir, "workspace-root", "li", "tools",
	)
	cursorLastDir := filepath.Join(cursorProject, "frontend")
	require.NoError(t, os.MkdirAll(cursorLastDir, 0o755))
	require.NoError(t, os.MkdirAll(
		filepath.Join(tmpDir, "workspace-root", "li-tools"),
		0o755,
	))

	cursorLastDirJSON, _ := json.Marshal(cursorLastDir)
	cursorTranscript := filepath.Join(
		tmpDir, ".cursor", "projects",
		encodeCursorProjectPathForTest(cursorProject),
		"agent-transcripts", "cursor-sess-last",
		"cursor-sess-last.jsonl",
	)
	require.NoError(t, os.MkdirAll(
		filepath.Dir(cursorTranscript), 0o755,
	))
	lastDirContent := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"pwd","working_directory":` +
		string(cursorLastDirJSON) + `}}]}}` + "\n"
	require.NoError(t, os.WriteFile(
		cursorTranscript, []byte(lastDirContent), 0o644,
	))

	got := resolveCursorWorkspaceDir(&db.Session{
		Agent:    "cursor",
		Project:  cursorProject,
		FilePath: &cursorTranscript,
	})
	assertSameDir(t, "resolveCursorWorkspaceDir()", got, cursorProject)
}

func TestResolveCursorWorkspaceDirAmbiguousWithoutHintReturnsEmpty(
	t *testing.T,
) {
	tmpDir := t.TempDir()

	// Create two paths that decode from the same encoded name.
	pathA := filepath.Join(tmpDir, "li-tools")
	pathB := filepath.Join(tmpDir, "li", "tools")
	require.NoError(t, os.MkdirAll(pathA, 0o755))
	require.NoError(t, os.MkdirAll(pathB, 0o755))

	encoded := encodeCursorProjectPathForTest(pathA)
	cursorTranscript := filepath.Join(
		tmpDir, ".cursor", "projects",
		encoded,
		"agent-transcripts", "cursor-sess",
		"cursor-sess.jsonl",
	)

	got := resolveCursorWorkspaceDir(&db.Session{
		Agent:    "cursor",
		Project:  "li_tools", // Not absolute — no hint.
		FilePath: &cursorTranscript,
	})
	assert.Empty(t, got,
		"resolveCursorWorkspaceDir() = %q, want empty "+
			"(ambiguous without hint)", got)
}

func TestResolveCursorWorkspaceDirStaleHintReturnsEmpty(
	t *testing.T,
) {
	tmpDir := t.TempDir()

	pathA := filepath.Join(tmpDir, "li-tools")
	pathB := filepath.Join(tmpDir, "li", "tools")
	require.NoError(t, os.MkdirAll(pathA, 0o755))
	require.NoError(t, os.MkdirAll(pathB, 0o755))

	// Stale hint: exists on disk but not under either candidate.
	staleDir := filepath.Join(tmpDir, "unrelated-project")
	require.NoError(t, os.MkdirAll(staleDir, 0o755))

	encoded := encodeCursorProjectPathForTest(pathA)
	staleDirJSON, _ := json.Marshal(staleDir)
	cursorTranscript := filepath.Join(
		tmpDir, ".cursor", "projects",
		encoded,
		"agent-transcripts", "cursor-sess",
		"cursor-sess.jsonl",
	)
	require.NoError(t, os.MkdirAll(
		filepath.Dir(cursorTranscript), 0o755,
	))
	content := `{"role":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"Shell","input":{` +
		`"command":"pwd","working_directory":` +
		string(staleDirJSON) + `}}]}}` + "\n"
	require.NoError(t, os.WriteFile(
		cursorTranscript, []byte(content), 0o644,
	))

	got := resolveCursorWorkspaceDir(&db.Session{
		Agent:    "cursor",
		Project:  "li_tools",
		FilePath: &cursorTranscript,
	})
	assert.Empty(t, got,
		"resolveCursorWorkspaceDir() with stale hint = %q, want empty",
		got)
}

func TestResolveCursorWorkspaceDirWithoutTranscriptContents(
	t *testing.T,
) {
	tmpDir := t.TempDir()

	cursorProject := filepath.Join(
		tmpDir, "workspace-root", "li-openhouse",
	)
	require.NoError(t, os.MkdirAll(cursorProject, 0o755))

	cursorTranscript := filepath.Join(
		tmpDir, ".cursor", "projects",
		encodeCursorProjectPathForTest(cursorProject),
		"agent-transcripts", "cursor-sess",
		"cursor-sess.jsonl",
	)

	got := resolveCursorWorkspaceDir(&db.Session{
		Agent:    "cursor",
		Project:  cursorProject,
		FilePath: &cursorTranscript,
	})
	assertSameDir(t, "resolveCursorWorkspaceDir()", got, cursorProject)
}

func TestResolveCursorResumePathsUsesProvidedLastWorkingDir(
	t *testing.T,
) {
	tmpDir := t.TempDir()

	cursorProject := filepath.Join(
		tmpDir, "workspace-root", "li", "tools",
	)
	cursorLastDir := filepath.Join(cursorProject, "frontend")
	require.NoError(t, os.MkdirAll(cursorLastDir, 0o755))
	require.NoError(t, os.MkdirAll(
		filepath.Join(tmpDir, "workspace-root", "li-tools"),
		0o755,
	))

	cursorTranscript := filepath.Join(
		tmpDir, ".cursor", "projects",
		encodeCursorProjectPathForTest(cursorProject),
		"agent-transcripts", "cursor-sess-last",
		"cursor-sess-last.jsonl",
	)

	launchDir, workspaceDir := resolveCursorResumePaths(
		&db.Session{
			Agent:    "cursor",
			Project:  cursorProject,
			FilePath: &cursorTranscript,
		},
		cursorLastDir,
	)
	assertSameDir(t, "launchDir", launchDir, cursorLastDir)
	assertSameDir(t, "workspaceDir", workspaceDir, cursorProject)
}

func TestResolveCursorResumePathsFallbackWorkspaceToLastWorkingDir(
	t *testing.T,
) {
	lastCwd := filepath.Join(t.TempDir(), "frontend")
	require.NoError(t, os.MkdirAll(lastCwd, 0o755))

	launchDir, workspaceDir := resolveCursorResumePaths(
		&db.Session{
			Agent:    "cursor",
			Project:  "li_tools",
			FilePath: nil,
		},
		lastCwd,
	)
	assertSameDir(t, "launchDir", launchDir, lastCwd)
	assertSameDir(t, "workspaceDir", workspaceDir, lastCwd)
}

func TestResolveResumeDirCanonicalizesSymlink(t *testing.T) {
	tmpDir := t.TempDir()

	realProject := filepath.Join(tmpDir, "repos", "openhouse")
	require.NoError(t, os.MkdirAll(realProject, 0o755))
	cacheDir := filepath.Join(tmpDir, "project_cache_hdfs")
	require.NoError(t, os.MkdirAll(cacheDir, 0o755))
	linkProject := filepath.Join(cacheDir, "openhouse")
	if err := os.Symlink(realProject, linkProject); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	linkJSON, _ := json.Marshal(linkProject)
	sessionFile := filepath.Join(tmpDir, "cursor-symlink.jsonl")
	content := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"pwd","working_directory":` +
		string(linkJSON) + `}}]}}` + "\n"
	require.NoError(t, os.WriteFile(sessionFile, []byte(content), 0o644))

	got := resolveResumeDir(&db.Session{
		Agent:    "cursor",
		Project:  "li_openhouse",
		FilePath: &sessionFile,
	})
	assertSameDir(t, "resolveResumeDir()", got, realProject)
}

func TestResolveSessionDirCursorProjectFallbackCanonicalizesSymlink(t *testing.T) {
	tmpDir := t.TempDir()

	realProject := filepath.Join(tmpDir, "repos", "openhouse")
	require.NoError(t, os.MkdirAll(realProject, 0o755))
	cacheDir := filepath.Join(tmpDir, "project_cache_hdfs")
	require.NoError(t, os.MkdirAll(cacheDir, 0o755))
	linkProject := filepath.Join(cacheDir, "openhouse")
	if err := os.Symlink(realProject, linkProject); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	got := resolveSessionDir(&db.Session{
		Agent:   "cursor",
		Project: linkProject,
	})
	assertSameDir(t, "resolveSessionDir()", got, realProject)
}

func TestResumeLaunchCwd(t *testing.T) {
	cwd := t.TempDir()

	tests := []struct {
		name     string
		agent    string
		openerID string
		goos     string
		want     string
	}{
		{
			name:     "claude keeps cwd for auto darwin launch",
			agent:    "claude",
			openerID: "auto",
			goos:     "darwin",
			want:     cwd,
		},
		{
			name:     "cursor auto darwin launch keeps cwd",
			agent:    "cursor",
			openerID: "auto",
			goos:     "darwin",
			want:     cwd,
		},
		{
			name:     "cursor iterm2 darwin launch keeps cwd",
			agent:    "cursor",
			openerID: "iterm2",
			goos:     "darwin",
			want:     cwd,
		},
		{
			name:     "cursor terminal darwin launch keeps cwd",
			agent:    "cursor",
			openerID: "terminal",
			goos:     "darwin",
			want:     cwd,
		},
		{
			name:     "cursor ghostty darwin launch keeps cwd",
			agent:    "cursor",
			openerID: "ghostty",
			goos:     "darwin",
			want:     cwd,
		},
		{
			name:     "cursor kitty darwin launch keeps cwd flag",
			agent:    "cursor",
			openerID: "kitty",
			goos:     "darwin",
			want:     cwd,
		},
		{
			name:     "cursor linux launch keeps cwd",
			agent:    "cursor",
			openerID: "ghostty",
			goos:     "linux",
			want:     cwd,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resumeLaunchCwd(
				tt.agent, tt.openerID, tt.goos, cwd,
			)
			assert.Equal(t, tt.want, got)
		})
	}
}

func encodeCursorProjectPathForTest(path string) string {
	clean := filepath.Clean(path)
	var tokens []string
	if volume := filepath.VolumeName(clean); volume != "" {
		tokens = append(tokens, strings.TrimSuffix(volume, ":"))
		clean = strings.TrimPrefix(clean, volume)
	}
	parts := strings.SplitSeq(clean, string(filepath.Separator))
	for part := range parts {
		if part == "" {
			continue
		}
		tokens = append(tokens, cursorComponentTokens(part)...)
	}
	return strings.Join(tokens, "-")
}
