package parser

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
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
			if gotCtx != tt.wantCtx || gotOut != tt.wantOut {
				t.Errorf(
					"InferTokenPresence() = (%v, %v), want (%v, %v)",
					gotCtx, gotOut, tt.wantCtx, tt.wantOut,
				)
			}
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
		if ok != tt.want {
			t.Errorf(
				"AgentByType(%q) ok = %v, want %v",
				tt.input, ok, tt.want,
			)
		}
		if ok && def.Type != tt.input {
			t.Errorf(
				"AgentByType(%q).Type = %q",
				tt.input, def.Type,
			)
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
			if ok != tt.wantOK {
				t.Fatalf(
					"AgentByPrefix(%q) ok = %v, want %v",
					tt.sessionID, ok, tt.wantOK,
				)
			}
			if ok && def.Type != tt.wantType {
				t.Errorf(
					"AgentByPrefix(%q).Type = %q, want %q",
					tt.sessionID, def.Type, tt.wantType,
				)
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
	}

	registered := make(map[AgentType]bool)
	for _, def := range Registry {
		registered[def.Type] = true
	}

	for _, at := range allTypes {
		if !registered[at] {
			t.Errorf(
				"AgentType %q missing from Registry", at,
			)
		}
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
			if len(tt.inputs) != len(tt.want) {
				t.Fatalf("len(inputs) = %d, want %d", len(tt.inputs), len(tt.want))
			}
			for i, r := range tt.inputs {
				if r.Session.RelationshipType != tt.want[i] {
					t.Errorf(
						"inputs[%d].RelationshipType = %q, want %q",
						i, r.Session.RelationshipType, tt.want[i],
					)
				}
			}
		})
	}
}

func TestFileBasedAgentsHaveConfigKey(t *testing.T) {
	for _, def := range Registry {
		if !def.FileBased {
			continue
		}
		if def.ConfigKey == "" {
			t.Errorf(
				"file-based agent %q (%s) has empty ConfigKey",
				def.DisplayName, def.Type,
			)
		}
	}
}

func TestOpenCodeRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentOpenCode)
	if !ok {
		t.Fatalf("AgentOpenCode missing from Registry")
	}
	if !def.FileBased {
		t.Fatalf("OpenCode FileBased = false, want true")
	}
	if def.DiscoverFunc == nil {
		t.Fatalf("OpenCode DiscoverFunc = nil")
	}
	if def.FindSourceFunc == nil {
		t.Fatalf("OpenCode FindSourceFunc = nil")
	}
	if got, want := def.WatchSubdirs, []string{
		"storage/session",
		"storage/message",
		"storage/part",
	}; !slices.Equal(got, want) {
		t.Fatalf("OpenCode WatchSubdirs = %v, want %v", got, want)
	}
}

func TestResolveOpenCodeSourcePrefersStorage(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "storage", "session", "global")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	dbPath := filepath.Join(root, "opencode.db")
	if err := os.WriteFile(dbPath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write db marker: %v", err)
	}

	got := ResolveOpenCodeSource(root)
	if got.Mode != OpenCodeSourceStorage {
		t.Fatalf("Mode = %v, want %v", got.Mode, OpenCodeSourceStorage)
	}
	if got.SessionRoot != filepath.Join(root, "storage", "session") {
		t.Fatalf("SessionRoot = %q", got.SessionRoot)
	}
}

func TestResolveOpenCodeSourceFallsBackToSQLiteOnBrokenStoragePath(
	t *testing.T,
) {
	root := t.TempDir()
	storagePath := filepath.Join(root, "storage")
	if err := os.WriteFile(storagePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write storage marker: %v", err)
	}
	dbPath := filepath.Join(root, "opencode.db")
	if err := os.WriteFile(dbPath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write db marker: %v", err)
	}

	got := ResolveOpenCodeSource(root)
	if got.Mode != OpenCodeSourceSQLite {
		t.Fatalf("Mode = %v, want %v", got.Mode, OpenCodeSourceSQLite)
	}
	if got.DBPath != dbPath {
		t.Fatalf("DBPath = %q, want %q", got.DBPath, dbPath)
	}
}

func TestResolveOpenCodeSourceKeepsStorageAuthoritativeWhenUnreadable(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on Windows")
	}
	root := t.TempDir()
	sessionDir := filepath.Join(root, "storage", "session", "global")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	storageRoot := filepath.Join(root, "storage")
	if err := os.Chmod(storageRoot, 0o000); err != nil {
		t.Fatalf("chmod storage root: %v", err)
	}
	defer func() {
		_ = os.Chmod(storageRoot, 0o755)
	}()
	dbPath := filepath.Join(root, "opencode.db")
	if err := os.WriteFile(dbPath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write db marker: %v", err)
	}

	got := ResolveOpenCodeSource(root)
	if got.Mode != OpenCodeSourceStorage {
		t.Fatalf("Mode = %v, want %v", got.Mode, OpenCodeSourceStorage)
	}
	if got.SessionRoot != filepath.Join(root, "storage", "session") {
		t.Fatalf("SessionRoot = %q", got.SessionRoot)
	}
}

func TestDiscoverOpenCodeSessions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "storage", "session", "global")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "ses_test.json")
	data := []byte(`{"id":"ses_test","directory":"/home/user/code/my-app"}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	got := DiscoverOpenCodeSessions(root)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Path != path {
		t.Fatalf("Path = %q, want %q", got[0].Path, path)
	}
	if got[0].Project != "my_app" {
		t.Fatalf("Project = %q, want %q", got[0].Project, "my_app")
	}
	if got[0].Agent != AgentOpenCode {
		t.Fatalf("Agent = %q, want %q", got[0].Agent, AgentOpenCode)
	}
}

func TestDiscoverOpenCodeSessionsIgnoresNestedJSON(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "storage", "session", "global")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "ses_test.json")
	if err := os.WriteFile(path, []byte(`{"id":"ses_test"}`), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "meta.json"), []byte(`{"id":"meta"}`), 0o644); err != nil {
		t.Fatalf("write nested json: %v", err)
	}

	got := DiscoverOpenCodeSessions(root)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Path != path {
		t.Fatalf("Path = %q, want %q", got[0].Path, path)
	}
}

func TestFindOpenCodeSourceFilePrefersStorage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "storage", "session", "global", "ses_123.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"id":"ses_123"}`), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "opencode.db"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write db marker: %v", err)
	}

	got := FindOpenCodeSourceFile(root, "ses_123")
	if got != path {
		t.Fatalf("FindOpenCodeSourceFile() = %q, want %q", got, path)
	}
}

func TestResolveOpenCodeWatchRootsStorage(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(
		filepath.Join(root, "storage", "session", "global"),
		0o755,
	); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}

	got := ResolveOpenCodeWatchRoots(root)
	want := []string{root}
	if !slices.Equal(got, want) {
		t.Fatalf("ResolveOpenCodeWatchRoots() = %v, want %v", got, want)
	}
}

func TestResolveOpenCodeWatchRootsSQLite(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "opencode.db"), []byte("x"), 0o644,
	); err != nil {
		t.Fatalf("write db marker: %v", err)
	}

	got := ResolveOpenCodeWatchRoots(root)
	want := []string{root}
	if !slices.Equal(got, want) {
		t.Fatalf("ResolveOpenCodeWatchRoots() = %v, want %v", got, want)
	}
}

func TestResolveOpenCodeWatchRootsMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	got := ResolveOpenCodeWatchRoots(root)
	if got != nil {
		t.Fatalf("ResolveOpenCodeWatchRoots() = %v, want nil", got)
	}
}

func TestParseOpenCodeSQLiteVirtualPath(t *testing.T) {
	dbPath := filepath.Join("/tmp", "opencode.db")
	virtual := OpenCodeSQLiteVirtualPath(dbPath, "ses_123")
	gotDB, gotSessionID, ok := ParseOpenCodeSQLiteVirtualPath(virtual)
	if !ok {
		t.Fatal("expected virtual path to parse")
	}
	if gotDB != dbPath || gotSessionID != "ses_123" {
		t.Fatalf("ParseOpenCodeSQLiteVirtualPath() = (%q, %q), want (%q, %q)", gotDB, gotSessionID, dbPath, "ses_123")
	}
	hashDBPath := filepath.Join("/tmp", "opencode#dev", "opencode.db")
	hashVirtual := OpenCodeSQLiteVirtualPath(hashDBPath, "ses_456")
	gotDB, gotSessionID, ok = ParseOpenCodeSQLiteVirtualPath(hashVirtual)
	if !ok {
		t.Fatal("expected virtual path with # in db path to parse")
	}
	if gotDB != hashDBPath || gotSessionID != "ses_456" {
		t.Fatalf("ParseOpenCodeSQLiteVirtualPath() with # in db path = (%q, %q), want (%q, %q)", gotDB, gotSessionID, hashDBPath, "ses_456")
	}
	if _, _, ok := ParseOpenCodeSQLiteVirtualPath("/tmp/project#dir/storage/session/global/ses_123.json"); ok {
		t.Fatal("expected real storage path with # to be rejected")
	}
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
			if host != tt.wantHost {
				t.Errorf(
					"StripHostPrefix(%q) host = %q, want %q",
					tt.id, host, tt.wantHost,
				)
			}
			if raw != tt.wantRaw {
				t.Errorf(
					"StripHostPrefix(%q) raw = %q, want %q",
					tt.id, raw, tt.wantRaw,
				)
			}
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
			if ok != tt.wantOK {
				t.Fatalf(
					"AgentByPrefix(%q) ok = %v, want %v",
					tt.sessionID, ok, tt.wantOK,
				)
			}
			if ok && def.Type != tt.wantType {
				t.Errorf(
					"AgentByPrefix(%q).Type = %q, want %q",
					tt.sessionID, def.Type, tt.wantType,
				)
			}
		})
	}
}

func TestVSCodeCopilotDefaultDirs(t *testing.T) {
	def, ok := AgentByType(AgentVSCodeCopilot)
	if !ok {
		t.Fatal("AgentVSCodeCopilot not in Registry")
	}

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
		if !slices.Contains(def.DefaultDirs, path) {
			t.Errorf("missing default dir: %s", path)
		}
	}
}
