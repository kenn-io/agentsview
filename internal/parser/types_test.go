package parser

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInferTokenPresence(t *testing.T) {
	tests := []struct {
		name        string
		tokenUsage  []byte
		contextToks int
		outputToks  int
		hasContext  bool
		hasOutput   bool
		wantCtx     bool
		wantOut     bool
	}{
		{
			name:       "explicit flags preserved, no data",
			hasContext: true,
			hasOutput:  true,
			wantCtx:    true,
			wantOut:    true,
		},
		{
			name:        "non-zero contextTokens infers presence",
			contextToks: 1000,
			wantCtx:     true,
			wantOut:     false,
		},
		{
			name:       "non-zero outputTokens infers presence",
			outputToks: 42,
			wantCtx:    false,
			wantOut:    true,
		},
		{
			name:    "zero numerics, no flags -> false/false",
			wantCtx: false,
			wantOut: false,
		},
		{
			name:       "json input_tokens key",
			tokenUsage: []byte(`{"input_tokens": 100}`),
			wantCtx:    true,
			wantOut:    false,
		},
		{
			name:       "json output_tokens key",
			tokenUsage: []byte(`{"output_tokens": 50}`),
			wantCtx:    false,
			wantOut:    true,
		},
		{
			name:       "json cache_read_input_tokens key",
			tokenUsage: []byte(`{"cache_read_input_tokens": 200}`),
			wantCtx:    true,
			wantOut:    false,
		},
		{
			name:       "json cache_creation_input_tokens key",
			tokenUsage: []byte(`{"cache_creation_input_tokens": 10}`),
			wantCtx:    true,
			wantOut:    false,
		},
		{
			name:       "json both sides",
			tokenUsage: []byte(`{"input_tokens": 100, "output_tokens": 50}`),
			wantCtx:    true,
			wantOut:    true,
		},
		{
			name:       "malformed json ignored",
			tokenUsage: []byte(`not-json`),
			wantCtx:    false,
			wantOut:    false,
		},
		{
			name:       "empty json object",
			tokenUsage: []byte(`{}`),
			wantCtx:    false,
			wantOut:    false,
		},
		{
			name:       "gemini style input key",
			tokenUsage: []byte(`{"input": 300}`),
			wantCtx:    true,
			wantOut:    false,
		},
		{
			name:       "gemini style output key",
			tokenUsage: []byte(`{"output": 75}`),
			wantCtx:    false,
			wantOut:    true,
		},
		{
			name:       "context_tokens json key",
			tokenUsage: []byte(`{"context_tokens": 500}`),
			wantCtx:    true,
			wantOut:    false,
		},
		{
			name:       "cached json key",
			tokenUsage: []byte(`{"cached": 30}`),
			wantCtx:    true,
			wantOut:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCtx, gotOut := InferTokenPresence(
				tt.tokenUsage,
				tt.contextToks,
				tt.outputToks,
				tt.hasContext,
				tt.hasOutput,
			)
			assert.Equal(t, tt.wantCtx, gotCtx, "InferTokenPresence context")
			assert.Equal(t, tt.wantOut, gotOut, "InferTokenPresence output")
		})
	}
}

func TestAgentByType(t *testing.T) {
	tests := []struct {
		input AgentType
		want  bool
	}{
		{AgentClaude, true},
		{AgentCodex, true},
		{AgentCopilot, true},
		{AgentGemini, true},
		{AgentMiMoCode, true},
		{AgentOpenCode, true},
		{AgentOpenHands, true},
		{AgentCursor, true},
		{AgentAmp, true},
		{AgentVSCodeCopilot, true},
		{AgentPi, true},
		{AgentOMP, true},
		{AgentDeepSeekTUI, true},
		{"unknown", false},
	}
	for _, tt := range tests {
		def, ok := AgentByType(tt.input)
		assert.Equalf(t, tt.want, ok, "AgentByType(%q) ok", tt.input)
		if ok {
			assert.Equalf(t, tt.input, def.Type, "AgentByType(%q).Type", tt.input)
		}
	}
}

func TestAgentByPrefix(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		wantType  AgentType
		wantOK    bool
	}{
		{
			"claude no prefix",
			"abc-123",
			AgentClaude,
			true,
		},
		{
			"codex prefix",
			"codex:some-uuid",
			AgentCodex,
			true,
		},
		{
			"copilot prefix",
			"copilot:sess-id",
			AgentCopilot,
			true,
		},
		{
			"gemini prefix",
			"gemini:sess-id",
			AgentGemini,
			true,
		},
		{
			"mimocode prefix",
			"mimocode:sess-id",
			AgentMiMoCode,
			true,
		},
		{
			"opencode prefix",
			"opencode:sess-id",
			AgentOpenCode,
			true,
		},
		{
			"openhands prefix",
			"openhands:sess-id",
			AgentOpenHands,
			true,
		},
		{
			"cursor prefix",
			"cursor:sess-id",
			AgentCursor,
			true,
		},
		{
			"amp prefix",
			"amp:T-019ca26f",
			AgentAmp,
			true,
		},
		{
			"vscode-copilot prefix",
			"vscode-copilot:sess-id",
			AgentVSCodeCopilot,
			true,
		},
		{
			"visualstudio-copilot prefix",
			"visualstudio-copilot:sess-id",
			AgentVSCopilot,
			true,
		},
		{
			"pi prefix",
			"pi:pi-session-uuid",
			AgentPi,
			true,
		},
		{
			"omp prefix",
			"omp:omp-session-uuid",
			AgentOMP,
			true,
		},
		{
			"zed prefix",
			"zed:sess-id",
			AgentZed,
			true,
		},
		{
			"qwenpaw prefix",
			"qwenpaw:default:sess-id",
			AgentQwenPaw,
			true,
		},
		{
			// Lock in the disjoint prefix: "qwenpaw:" must NOT be
			// swallowed by the "qwen:" rule (no shared stem), so
			// QwenPaw IDs never route to the Qwen agent.
			"qwen prefix does not capture qwenpaw",
			"qwen:sess-id",
			AgentQwen,
			true,
		},
		{
			"deepseek tui prefix",
			"deepseek-tui:sess-id",
			AgentDeepSeekTUI,
			true,
		},
		{
			"remote deepseek tui prefix",
			"devbox~deepseek-tui:sess-id",
			AgentDeepSeekTUI,
			true,
		},
		{
			"unknown prefix",
			"future:sess-id",
			"",
			false,
		},
		{
			"empty string",
			"",
			AgentClaude,
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def, ok := AgentByPrefix(tt.sessionID)
			require.Equalf(t, tt.wantOK, ok, "AgentByPrefix(%q) ok", tt.sessionID)
			if ok {
				assert.Equalf(t, tt.wantType, def.Type,
					"AgentByPrefix(%q).Type", tt.sessionID)
			}
		})
	}
}

func TestRegistryCompleteness(t *testing.T) {
	// allTypes is the canonical list of every supported agent. It must match
	// Registry exactly in both directions: the assertions below fail if an
	// agent is registered without being listed here (or vice versa), so a new
	// AgentDef cannot silently bypass this check the way several agents
	// previously did.
	allTypes := []AgentType{
		AgentClaude,
		AgentCowork,
		AgentCodex,
		AgentCopilot,
		AgentGemini,
		AgentMiMoCode,
		AgentOpenCode,
		AgentKilo,
		AgentOpenHands,
		AgentCursor,
		AgentAmp,
		AgentVSCodeCopilot,
		AgentVSCopilot,
		AgentPi,
		AgentOMP,
		AgentQwen,
		AgentCommandCode,
		AgentDeepSeekTUI,
		AgentOpenClaw,
		AgentQClaw,
		AgentKimi,
		AgentClaudeAI,
		AgentChatGPT,
		AgentKiro,
		AgentKiroIDE,
		AgentCortex,
		AgentHermes,
		AgentForge,
		AgentPiebald,
		AgentWarp,
		AgentPositron,
		AgentZed,
		AgentAntigravity,
		AgentAntigravityCLI,
		AgentIflow,
		AgentWorkBuddy,
		AgentZencoder,
		AgentGptme,
		AgentQwenPaw,
		AgentShelley,
		AgentVibe,
		AgentAider,
		AgentReasonix,
	}

	expected := make(map[AgentType]bool, len(allTypes))
	for _, at := range allTypes {
		assert.Falsef(t, expected[at], "AgentType %q listed more than once in allTypes", at)
		expected[at] = true
	}

	registered := make(map[AgentType]bool, len(Registry))
	for _, def := range Registry {
		assert.Falsef(t, registered[def.Type],
			"AgentType %q registered more than once in Registry", def.Type)
		registered[def.Type] = true
	}

	// Every listed agent must be registered.
	for at := range expected {
		assert.Truef(t, registered[at], "AgentType %q missing from Registry", at)
	}
	// Every registered agent must be listed, so additions to Registry cannot
	// silently skip this completeness check.
	for at := range registered {
		assert.Truef(t, expected[at],
			"AgentType %q registered but not listed in allTypes (add it to TestRegistryCompleteness)", at)
	}
}

func TestInferRelationshipTypes(t *testing.T) {
	tests := []struct {
		name   string
		inputs []ParseResult
		want   []RelationshipType
	}{{
		"no parent",
		[]ParseResult{
			{Session: ParsedSession{ID: "abc"}},
		},
		[]RelationshipType{RelNone},
	},
		{
			"agent prefix gets subagent",
			[]ParseResult{
				{Session: ParsedSession{
					ID:              "agent-123",
					ParentSessionID: "parent",
				}},
			},
			[]RelationshipType{RelSubagent},
		},
		{
			"non-agent prefix gets continuation",
			[]ParseResult{
				{Session: ParsedSession{
					ID:              "child-session",
					ParentSessionID: "parent",
				}},
			},
			[]RelationshipType{RelContinuation},
		},
		{
			"pi prefixed session with parent gets continuation",
			[]ParseResult{
				{Session: ParsedSession{
					ID:              "pi:branched-session",
					ParentSessionID: "pi:parent-session",
				}},
			},
			[]RelationshipType{RelContinuation},
		},
		{
			"explicit type preserved",
			[]ParseResult{
				{Session: ParsedSession{
					ID:               "abc-fork",
					ParentSessionID:  "parent",
					RelationshipType: RelFork,
				}},
			},
			[]RelationshipType{RelFork},
		},
		{
			"mixed results",
			[]ParseResult{
				{Session: ParsedSession{ID: "main"}},
				{Session: ParsedSession{
					ID:              "agent-task1",
					ParentSessionID: "main",
				}},
				{Session: ParsedSession{
					ID:               "main-fork-uuid",
					ParentSessionID:  "main",
					RelationshipType: RelFork,
				}},
				{Session: ParsedSession{
					ID:              "child",
					ParentSessionID: "main",
				}},
			},
			[]RelationshipType{
				RelNone, RelSubagent, RelFork, RelContinuation,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			InferRelationshipTypes(tt.inputs)
			require.Len(t, tt.inputs, len(tt.want), "inputs len")
			for i, r := range tt.inputs {
				assert.Equalf(t, tt.want[i], r.Session.RelationshipType,
					"inputs[%d].RelationshipType", i)
			}
		})
	}
}

func TestFileBasedAgentsHaveConfigKey(t *testing.T) {
	for _, def := range Registry {
		if !def.FileBased {
			continue
		}
		assert.NotEmptyf(t, def.ConfigKey,
			"file-based agent %q (%s) has empty ConfigKey",
			def.DisplayName, def.Type)
	}
}

func TestZedRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentZed)
	require.True(t, ok, "AgentZed missing from Registry")
	require.True(t, def.FileBased, "Zed FileBased")
	assert.Equal(t, "ZED_DIR", def.EnvVar)
	assert.Equal(t, "zed_dirs", def.ConfigKey)
	assert.Equal(t, "zed:", def.IDPrefix)
}

func TestShelleyRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentShelley)
	require.True(t, ok, "AgentShelley missing from Registry")
	require.True(t, def.FileBased, "Shelley FileBased")
	assert.Equal(t, "SHELLEY_DIR", def.EnvVar)
	assert.Equal(t, "shelley_dirs", def.ConfigKey)
	assert.Equal(t, "shelley:", def.IDPrefix)
}

func TestOpenCodeRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentOpenCode)
	require.True(t, ok, "AgentOpenCode missing from Registry")
	require.True(t, def.FileBased, "OpenCode FileBased")
	want := []string{
		"storage/session",
		"storage/message",
		"storage/part",
	}
	require.Truef(t, slices.Equal(def.WatchSubdirs, want),
		"OpenCode WatchSubdirs = %v, want %v", def.WatchSubdirs, want)
}

func TestCoworkRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentCowork)
	require.True(t, ok, "AgentCowork missing from Registry")
	require.True(t, def.FileBased, "Cowork FileBased")
	assert.Equal(t, "COWORK_DIR", def.EnvVar)
	assert.Equal(t, "cowork_dirs", def.ConfigKey)
	assert.Equal(t, "cowork:", def.IDPrefix)
	assert.Equal(t, coworkDefaultDirs(), def.DefaultDirs)
	assert.True(t, def.ShallowWatch,
		"Cowork root contains large local_* working trees that discovery skips")
}

func TestAgentByPrefixCowork(t *testing.T) {
	def, ok := AgentByPrefix("cowork:c0000000-0000-4000-8000-000000000001")
	require.True(t, ok, "cowork-prefixed ID should resolve")
	assert.Equal(t, AgentCowork, def.Type)
}

func TestMiMoCodeRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentMiMoCode)
	require.True(t, ok, "AgentMiMoCode missing from Registry")
	require.True(t, def.FileBased, "MiMoCode FileBased")
	assert.Equal(t, "MIMOCODE_DIR", def.EnvVar)
	assert.Equal(t, "mimocode_dirs", def.ConfigKey)
	assert.Equal(t, []string{".local/share/mimocode"}, def.DefaultDirs)
	assert.Equal(t, "mimocode:", def.IDPrefix)
	want := []string{
		"storage/session_diff",
		"storage/message",
		"storage/part",
	}
	require.Truef(t, slices.Equal(def.WatchSubdirs, want),
		"MiMoCode WatchSubdirs = %v, want %v", def.WatchSubdirs, want)
}

func TestCommandCodeRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentCommandCode)
	require.True(t, ok, "AgentCommandCode missing from Registry")
	require.True(t, def.FileBased, "Command Code FileBased")
	assert.Equal(t, []string{".commandcode/projects"}, def.DefaultDirs)
	assert.Equal(t, "commandcode:", def.IDPrefix)
}

func TestDeepSeekTUIRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentDeepSeekTUI)
	require.True(t, ok, "AgentDeepSeekTUI missing from Registry")
	require.True(t, def.FileBased, "DeepSeek TUI FileBased")
	assert.Equal(t, "DeepSeek TUI", def.DisplayName)
	assert.Equal(t, "DEEPSEEK_TUI_SESSIONS_DIR", def.EnvVar)
	assert.Equal(t, "deepseek_tui_sessions_dirs", def.ConfigKey)
	assert.Equal(t, []string{".codewhale/sessions", ".deepseek/sessions"}, def.DefaultDirs)
	assert.Equal(t, "deepseek-tui:", def.IDPrefix)
}

func TestResolveOpenCodeSourcePrefersStorage(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "storage", "session", "global")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755), "mkdir session dir")
	dbPath := filepath.Join(root, "opencode.db")
	require.NoError(t, os.WriteFile(dbPath, []byte("x"), 0o644), "write db marker")

	got := ResolveOpenCodeSource(root)
	require.Equal(t, OpenCodeSourceStorage, got.Mode, "Mode")
	require.Equal(t, filepath.Join(root, "storage", "session"), got.SessionRoot, "SessionRoot")
}

func TestResolveMiMoCodeSourcePrefersStorage(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "storage", "session_diff", "global")
	require.NoError(t, os.MkdirAll(dir, 0o755), "mkdir")
	dbPath := filepath.Join(root, "mimocode.db")
	require.NoError(t, os.WriteFile(dbPath, []byte("x"), 0o644), "write db marker")

	src := ResolveMiMoCodeSource(root)
	require.Equal(t, OpenCodeSourceStorage, src.Mode, "Mode")
	require.Equal(t, filepath.Join(root, "storage", "session_diff"), src.SessionRoot)

	path := filepath.Join(dir, "ses_test.json")
	require.NoError(t, os.WriteFile(path,
		[]byte(`{"id":"ses_test","directory":"/home/user/code/my-app"}`),
		0o644))

	discovered := discoverOpenCodeFormatSessions(mimoFmt, root)
	require.Len(t, discovered, 1)
	require.Equal(t, AgentMiMoCode, discovered[0].Agent)

	require.Equal(t, path, findOpenCodeFormatSourceFile(mimoFmt, root, "ses_test"))
}

func TestResolveOpenCodeSourceFallsBackToSQLiteOnBrokenStoragePath(
	t *testing.T,
) {
	root := t.TempDir()
	storagePath := filepath.Join(root, "storage")
	require.NoError(t, os.WriteFile(storagePath, []byte("x"), 0o644), "write storage marker")
	dbPath := filepath.Join(root, "opencode.db")
	require.NoError(t, os.WriteFile(dbPath, []byte("x"), 0o644), "write db marker")

	got := ResolveOpenCodeSource(root)
	require.Equal(t, OpenCodeSourceSQLite, got.Mode, "Mode")
	require.Equal(t, dbPath, got.DBPath, "DBPath")
}

func TestResolveOpenCodeSourceKeepsStorageAuthoritativeWhenUnreadable(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on Windows")
	}
	root := t.TempDir()
	sessionDir := filepath.Join(root, "storage", "session", "global")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755), "mkdir session dir")
	storageRoot := filepath.Join(root, "storage")
	require.NoError(t, os.Chmod(storageRoot, 0o000), "chmod storage root")
	defer func() {
		_ = os.Chmod(storageRoot, 0o755)
	}()
	dbPath := filepath.Join(root, "opencode.db")
	require.NoError(t, os.WriteFile(dbPath, []byte("x"), 0o644), "write db marker")

	got := ResolveOpenCodeSource(root)
	require.Equal(t, OpenCodeSourceStorage, got.Mode, "Mode")
	require.Equal(t, filepath.Join(root, "storage", "session"), got.SessionRoot, "SessionRoot")
}

func TestDiscoverOpenCodeSessions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "storage", "session", "global")
	require.NoError(t, os.MkdirAll(dir, 0o755), "mkdir")
	path := filepath.Join(dir, "ses_test.json")
	data := []byte(`{"id":"ses_test","directory":"/home/user/code/my-app"}`)
	require.NoError(t, os.WriteFile(path, data, 0o644), "write session")

	got := discoverOpenCodeFormatSessions(openCodeFmt, root)
	require.Len(t, got, 1, "len")
	require.Equal(t, path, got[0].Path, "Path")
	require.Equal(t, "my_app", got[0].Project, "Project")
	require.Equal(t, AgentOpenCode, got[0].Agent, "Agent")
}

func TestDiscoverOpenCodeSessionsIgnoresNestedJSON(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "storage", "session", "global")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nested"), 0o755), "mkdir")
	path := filepath.Join(dir, "ses_test.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"id":"ses_test"}`), 0o644), "write session")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nested", "meta.json"), []byte(`{"id":"meta"}`), 0o644), "write nested json")

	got := discoverOpenCodeFormatSessions(openCodeFmt, root)
	require.Len(t, got, 1, "len")
	require.Equal(t, path, got[0].Path, "Path")
}

func TestFindOpenCodeSourceFilePrefersStorage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "storage", "session", "global", "ses_123.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "mkdir")
	require.NoError(t, os.WriteFile(path, []byte(`{"id":"ses_123"}`), 0o644), "write session")
	require.NoError(t, os.WriteFile(filepath.Join(root, "opencode.db"), []byte("x"), 0o644), "write db marker")

	got := findOpenCodeFormatSourceFile(openCodeFmt, root, "ses_123")
	require.Equal(t, path, got, "FindOpenCodeSourceFile()")
}

func TestFindOpenCodeSourceFileFallsBackToSQLiteInHybridRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "storage", "session", "global"),
		0o755,
	), "mkdir session dir")
	dbPath := filepath.Join(root, "opencode.db")
	seedHybridSQLiteDB(t, dbPath, "ses_456")

	got := findOpenCodeFormatSourceFile(openCodeFmt, root, "ses_456")
	want := OpenCodeSQLiteVirtualPath(dbPath, "ses_456")
	require.Equal(t, want, got, "FindOpenCodeSourceFile()")
}

// TestFindOpenCodeSourceFileReturnsEmptyWhenSessionMissing covers
// the multi-root shadowing case: an early hybrid root with an
// opencode.db file that does NOT contain the session must return
// "" so the engine's FindSourceFile loop continues to later roots
// where the session actually lives.
func TestFindOpenCodeSourceFileReturnsEmptyWhenSessionMissing(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "storage", "session", "global"),
		0o755,
	), "mkdir session dir")
	dbPath := filepath.Join(root, "opencode.db")
	seedHybridSQLiteDB(t, dbPath, "ses_unrelated")

	got := findOpenCodeFormatSourceFile(openCodeFmt, root, "ses_missing")
	assert.Empty(t, got, "FindOpenCodeSourceFile()")
}

func TestFindOpenCodeSourceFilePureSQLiteOnlyForExistingSession(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "opencode.db")
	seedHybridSQLiteDB(t, dbPath, "ses_present")

	got := findOpenCodeFormatSourceFile(openCodeFmt, root, "ses_present")
	assert.Equal(t,
		OpenCodeSQLiteVirtualPath(dbPath, "ses_present"),
		got, "FindOpenCodeSourceFile(present)")
	got = findOpenCodeFormatSourceFile(openCodeFmt, root, "ses_absent")
	assert.Empty(t, got, "FindOpenCodeSourceFile(absent)")
}

func TestOpenCodeStorageSessionIDsCollectsJSONFiles(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "storage", "session")
	require.NoError(t, os.MkdirAll(
		filepath.Join(sessionDir, "global"), 0o755,
	), "mkdir global")
	require.NoError(t, os.MkdirAll(
		filepath.Join(sessionDir, "proj-x"), 0o755,
	), "mkdir proj-x")
	for _, p := range []string{
		filepath.Join(sessionDir, "global", "ses_a.json"),
		filepath.Join(sessionDir, "global", "ses_b.json"),
		filepath.Join(sessionDir, "proj-x", "ses_c.json"),
		filepath.Join(sessionDir, "global", "skip.txt"),
	} {
		require.NoErrorf(t, os.WriteFile(p, []byte("{}"), 0o644), "write %s", p)
	}

	got := OpenCodeStorageSessionIDs(root)
	want := map[string]struct{}{
		"ses_a": {},
		"ses_b": {},
		"ses_c": {},
	}
	require.Lenf(t, got, len(want), "got %v, want %v", got, want)
	for id := range want {
		_, ok := got[id]
		assert.Truef(t, ok, "missing %q in result %v", id, got)
	}
}

func TestOpenCodeStorageSessionIDsNilForNonStorageRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "opencode.db"), []byte("x"), 0o644,
	), "write db marker")
	got := OpenCodeStorageSessionIDs(root)
	assert.Nil(t, got, "want nil for SQLite-only root")
}

func TestResolveCodexShallowWatchRoots(t *testing.T) {
	tests := []struct {
		name string
		root string
		want []string
	}{
		{
			name: "sessions dir",
			root: filepath.Join("home", ".codex", "sessions"),
			want: []string{filepath.Join("home", ".codex")},
		},
		{
			name: "archived sessions dir",
			root: filepath.Join("home", ".codex", "archived_sessions"),
			want: []string{filepath.Join("home", ".codex")},
		},
		{
			name: "empty root",
			root: "",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveCodexShallowWatchRoots(tt.root)
			assert.Truef(t, slices.Equal(got, tt.want),
				"ResolveCodexShallowWatchRoots(%q) = %v, want %v",
				tt.root, got, tt.want)
		})
	}
}

func TestCodexDefShallowWatchesIndexParent(t *testing.T) {
	var def AgentDef
	found := false
	for _, d := range Registry {
		if d.Type == AgentCodex {
			def = d
			found = true
			break
		}
	}
	require.True(t, found, "Codex agent def must exist")
	require.NotNil(t, def.ShallowWatchRootsFunc,
		"Codex must watch its index parent shallowly")
	got := def.ShallowWatchRootsFunc(
		filepath.Join("home", ".codex", "sessions"),
	)
	want := []string{filepath.Join("home", ".codex")}
	assert.Truef(t, slices.Equal(got, want),
		"Codex ShallowWatchRootsFunc = %v, want %v", got, want)
}

func TestResolveOpenCodeWatchRootsStorage(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "storage", "session", "global"),
		0o755,
	), "mkdir session dir")

	got := ResolveOpenCodeWatchRoots(root)
	want := []string{filepath.Join(root, "storage")}
	assert.Truef(t, slices.Equal(got, want),
		"ResolveOpenCodeWatchRoots() = %v, want %v", got, want)
}

func TestResolveOpenCodeWatchRootsHybrid(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "storage", "session", "global"),
		0o755,
	), "mkdir session dir")
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "opencode.db"), []byte("x"), 0o644,
	), "write db marker")

	got := ResolveOpenCodeWatchRoots(root)
	want := []string{root}
	assert.Truef(t, slices.Equal(got, want),
		"ResolveOpenCodeWatchRoots() = %v, want %v", got, want)
}

// A fresh opencode install may only have storage/session at startup;
// message/ and part/ get created lazily when the first message is
// written. Returning storage/ as the watch root ensures the watcher's
// Create handler picks up those lazy subdirs without a restart.
func TestResolveOpenCodeWatchRootsStorageMissingSubdirs(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "storage", "session"),
		0o755,
	), "mkdir session dir")

	got := ResolveOpenCodeWatchRoots(root)
	want := []string{filepath.Join(root, "storage")}
	assert.Truef(t, slices.Equal(got, want),
		"ResolveOpenCodeWatchRoots() = %v, want %v", got, want)
}

func TestResolveOpenCodeWatchRootsSQLite(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "opencode.db"), []byte("x"), 0o644,
	), "write db marker")

	got := ResolveOpenCodeWatchRoots(root)
	want := []string{root}
	assert.Truef(t, slices.Equal(got, want),
		"ResolveOpenCodeWatchRoots() = %v, want %v", got, want)
}

func TestResolveOpenCodeWatchRootsMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	got := ResolveOpenCodeWatchRoots(root)
	assert.Nil(t, got, "ResolveOpenCodeWatchRoots()")
}

func TestParseOpenCodeSQLiteVirtualPath(t *testing.T) {
	dbPath := filepath.Join("/tmp", "opencode.db")
	virtual := OpenCodeSQLiteVirtualPath(dbPath, "ses_123")
	gotDB, gotSessionID, ok := parseOpenCodeFormatVirtualPath(openCodeFmt.dbName, virtual)
	require.True(t, ok, "expected virtual path to parse")
	assert.Equal(t, dbPath, gotDB, "db path")
	assert.Equal(t, "ses_123", gotSessionID, "session ID")
	hashDBPath := filepath.Join("/tmp", "opencode#dev", "opencode.db")
	hashVirtual := OpenCodeSQLiteVirtualPath(hashDBPath, "ses_456")
	gotDB, gotSessionID, ok = parseOpenCodeFormatVirtualPath(openCodeFmt.dbName, hashVirtual)
	require.True(t, ok, "expected virtual path with # in db path to parse")
	assert.Equal(t, hashDBPath, gotDB, "db path with #")
	assert.Equal(t, "ses_456", gotSessionID, "session ID with #")
	_, _, ok = parseOpenCodeFormatVirtualPath(
		openCodeFmt.dbName,
		"/tmp/project#dir/storage/session/global/ses_123.json",
	)
	assert.False(t, ok, "expected real storage path with # to be rejected")
}

func TestStripHostPrefix(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		wantHost string
		wantRaw  string
	}{
		{
			"local claude id",
			"abc-123-def",
			"",
			"abc-123-def",
		},
		{
			"local codex id",
			"codex:some-uuid",
			"",
			"codex:some-uuid",
		},
		{
			"host-prefixed claude",
			"devbox1~abc-123-def",
			"devbox1",
			"abc-123-def",
		},
		{
			"host-prefixed codex",
			"devbox1~codex:some-uuid",
			"devbox1",
			"codex:some-uuid",
		},
		{
			"host-prefixed copilot",
			"server2~copilot:sess-id",
			"server2",
			"copilot:sess-id",
		},
		{
			"fqdn host",
			"dev.example.com~abc-123",
			"dev.example.com",
			"abc-123",
		},
		{
			"empty string",
			"",
			"",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, raw := StripHostPrefix(tt.id)
			assert.Equalf(t, tt.wantHost, host, "StripHostPrefix(%q) host", tt.id)
			assert.Equalf(t, tt.wantRaw, raw, "StripHostPrefix(%q) raw", tt.id)
		})
	}
}

func TestAgentByPrefixRemote(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		wantType  AgentType
		wantOK    bool
	}{
		{
			"remote claude",
			"devbox1~abc-123",
			AgentClaude,
			true,
		},
		{
			"remote codex",
			"devbox1~codex:some-uuid",
			AgentCodex,
			true,
		},
		{
			"remote copilot",
			"server2~copilot:sess-id",
			AgentCopilot,
			true,
		},
		{
			"remote gemini",
			"myhost~gemini:sess-id",
			AgentGemini,
			true,
		},
		{
			"fqdn host with claude",
			"dev.example.com~abc-123",
			AgentClaude,
			true,
		},
		{
			"fqdn host with codex",
			"prod.example.com~codex:sess-id",
			AgentCodex,
			true,
		},
		{
			"remote unknown agent",
			"host1~future:sess-id",
			"",
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def, ok := AgentByPrefix(tt.sessionID)
			require.Equalf(t, tt.wantOK, ok, "AgentByPrefix(%q) ok", tt.sessionID)
			if ok {
				assert.Equalf(t, tt.wantType, def.Type,
					"AgentByPrefix(%q).Type", tt.sessionID)
			}
		})
	}
}

func TestVSCodeCopilotDefaultDirs(t *testing.T) {
	def, ok := AgentByType(AgentVSCodeCopilot)
	require.True(t, ok, "AgentVSCodeCopilot not in Registry")

	required := []string{
		// Windows
		"AppData/Roaming/Code/User",
		"AppData/Roaming/Code - Insiders/User",
		"AppData/Roaming/VSCodium/User",
		// macOS
		"Library/Application Support/Code/User",
		"Library/Application Support/Code - Insiders/User",
		"Library/Application Support/VSCodium/User",
		// Linux
		".config/Code/User",
		".config/Code - Insiders/User",
		".config/VSCodium/User",
	}
	for _, path := range required {
		assert.Truef(t, slices.Contains(def.DefaultDirs, path),
			"missing default dir: %s", path)
	}
}

func TestApplyUsageEventTokenTotals(t *testing.T) {
	// Verify that applyUsageEventTokenTotals computes PeakContextTokens
	// correctly including cache-creation and cache-read tokens.
	sess := &ParsedSession{}
	events := []ParsedUsageEvent{
		{
			InputTokens:              1000,
			OutputTokens:             200,
			CacheReadInputTokens:     500,
			CacheCreationInputTokens: 300,
		},
		{
			InputTokens:              800,
			OutputTokens:             150,
			CacheReadInputTokens:     1200,
			CacheCreationInputTokens: 100,
		},
	}

	applyUsageEventTokenTotals(sess, events)

	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 350, sess.TotalOutputTokens)

	assert.True(t, sess.HasPeakContextTokens)
	// Peak context should be max of context window (InputTokens + CacheRead + CacheCreation)
	// Event 1 context = 1000 + 500 + 300 = 1800
	// Event 2 context = 800 + 1200 + 100 = 2100
	assert.Equal(t, 2100, sess.PeakContextTokens)
}

func TestReasonixRegistryEntry(t *testing.T) {
	// Find Reasonix in the registry
	var reasonixDef *AgentDef
	for _, def := range Registry {
		if def.Type == AgentReasonix {
			reasonixDef = &def
			break
		}
	}
	require.NotNil(t, reasonixDef, "AgentReasonix must be in Registry")

	// Verify basic properties
	assert.Equal(t, AgentReasonix, reasonixDef.Type)
	assert.Equal(t, "Reasonix", reasonixDef.DisplayName)
	assert.Equal(t, "REASONIX_DIR", reasonixDef.EnvVar)
	assert.Equal(t, "reasonix_dirs", reasonixDef.ConfigKey)
	assert.Equal(t, "reasonix:", reasonixDef.IDPrefix)
	assert.True(t, reasonixDef.FileBased)

	// Verify watch subdirs
	assert.Contains(t, reasonixDef.WatchSubdirs, "sessions")
	assert.Contains(t, reasonixDef.WatchSubdirs, "archive")

	// Verify default dirs contain .reasonix and Windows path
	assert.True(t, len(reasonixDef.DefaultDirs) > 0)
	hasUnix := false
	hasWindows := false
	for _, dir := range reasonixDef.DefaultDirs {
		if dir == ".reasonix" {
			hasUnix = true
		}
		if dir == "AppData/Roaming/reasonix" {
			hasWindows = true
		}
	}
	assert.True(t, hasUnix, "DefaultDirs should contain .reasonix")
	assert.True(t, hasWindows, "DefaultDirs should contain AppData/Roaming/reasonix")
}
