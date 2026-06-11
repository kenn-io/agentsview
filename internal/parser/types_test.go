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
		{AgentOpenCode, true},
		{AgentOpenHands, true},
		{AgentCursor, true},
		{AgentAmp, true},
		{AgentVSCodeCopilot, true},
		{AgentPi, true},
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
			"pi prefix",
			"pi:pi-session-uuid",
			AgentPi,
			true,
		},
		{
			"zed prefix",
			"zed:sess-id",
			AgentZed,
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
	allTypes := []AgentType{
		AgentClaude,
		AgentCodex,
		AgentCopilot,
		AgentGemini,
		AgentOpenCode,
		AgentOpenHands,
		AgentCursor,
		AgentAmp,
		AgentVSCodeCopilot,
		AgentPi,
		AgentQwen,
		AgentCommandCode,
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
	}

	registered := make(map[AgentType]bool)
	for _, def := range Registry {
		registered[def.Type] = true
	}

	for _, at := range allTypes {
		assert.Truef(t, registered[at],
			"AgentType %q missing from Registry", at)
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
	if !ok {
		t.Fatalf("AgentZed missing from Registry")
	}
	if !def.FileBased {
		t.Fatalf("Zed FileBased = false, want true")
	}
	if def.EnvVar != "ZED_DIR" {
		t.Fatalf("Zed EnvVar = %q", def.EnvVar)
	}
	if def.ConfigKey != "zed_dirs" {
		t.Fatalf("Zed ConfigKey = %q", def.ConfigKey)
	}
	if def.IDPrefix != "zed:" {
		t.Fatalf("Zed IDPrefix = %q", def.IDPrefix)
	}
	if def.DiscoverFunc == nil || def.FindSourceFunc == nil {
		t.Fatalf("Zed discover/source funcs must be set")
	}
}

func TestOpenCodeRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentOpenCode)
	require.True(t, ok, "AgentOpenCode missing from Registry")
	require.True(t, def.FileBased, "OpenCode FileBased")
	require.NotNil(t, def.DiscoverFunc, "OpenCode DiscoverFunc")
	require.NotNil(t, def.FindSourceFunc, "OpenCode FindSourceFunc")
	want := []string{
		"storage/session",
		"storage/message",
		"storage/part",
	}
	require.Truef(t, slices.Equal(def.WatchSubdirs, want),
		"OpenCode WatchSubdirs = %v, want %v", def.WatchSubdirs, want)
}

func TestCommandCodeRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentCommandCode)
	require.True(t, ok, "AgentCommandCode missing from Registry")
	require.True(t, def.FileBased, "Command Code FileBased")
	require.NotNil(t, def.DiscoverFunc, "Command Code DiscoverFunc")
	require.NotNil(t, def.FindSourceFunc, "Command Code FindSourceFunc")
	assert.Equal(t, []string{".commandcode/projects"}, def.DefaultDirs)
	assert.Equal(t, "commandcode:", def.IDPrefix)
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

	got := DiscoverOpenCodeSessions(root)
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

	got := DiscoverOpenCodeSessions(root)
	require.Len(t, got, 1, "len")
	require.Equal(t, path, got[0].Path, "Path")
}

func TestFindOpenCodeSourceFilePrefersStorage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "storage", "session", "global", "ses_123.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "mkdir")
	require.NoError(t, os.WriteFile(path, []byte(`{"id":"ses_123"}`), 0o644), "write session")
	require.NoError(t, os.WriteFile(filepath.Join(root, "opencode.db"), []byte("x"), 0o644), "write db marker")

	got := FindOpenCodeSourceFile(root, "ses_123")
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

	got := FindOpenCodeSourceFile(root, "ses_456")
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

	got := FindOpenCodeSourceFile(root, "ses_missing")
	assert.Empty(t, got, "FindOpenCodeSourceFile()")
}

func TestFindOpenCodeSourceFilePureSQLiteOnlyForExistingSession(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "opencode.db")
	seedHybridSQLiteDB(t, dbPath, "ses_present")

	got := FindOpenCodeSourceFile(root, "ses_present")
	assert.Equal(t,
		OpenCodeSQLiteVirtualPath(dbPath, "ses_present"),
		got, "FindOpenCodeSourceFile(present)")
	got = FindOpenCodeSourceFile(root, "ses_absent")
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
	gotDB, gotSessionID, ok := ParseOpenCodeSQLiteVirtualPath(virtual)
	require.True(t, ok, "expected virtual path to parse")
	assert.Equal(t, dbPath, gotDB, "db path")
	assert.Equal(t, "ses_123", gotSessionID, "session ID")
	hashDBPath := filepath.Join("/tmp", "opencode#dev", "opencode.db")
	hashVirtual := OpenCodeSQLiteVirtualPath(hashDBPath, "ses_456")
	gotDB, gotSessionID, ok = ParseOpenCodeSQLiteVirtualPath(hashVirtual)
	require.True(t, ok, "expected virtual path with # in db path to parse")
	assert.Equal(t, hashDBPath, gotDB, "db path with #")
	assert.Equal(t, "ses_456", gotSessionID, "session ID with #")
	_, _, ok = ParseOpenCodeSQLiteVirtualPath(
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
