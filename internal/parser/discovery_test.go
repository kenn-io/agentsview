package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	copilotStateDir = "session-state"
	geminiChatsDir  = "chats"
)

// setupFileSystem creates a temporary directory and populates
// it with the given relative file paths and contents.
func setupFileSystem(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		fullPath := filepath.Join(dir, path)

		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(fullPath), err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", fullPath, err)
		}
	}
}

// assertDiscoveredFiles verifies that the discovered files match the expected filenames and agent type.
func assertDiscoveredFiles(t *testing.T, got []DiscoveredFile, wantFilenames []string, wantAgent AgentType) {
	t.Helper()

	want := make(map[string]bool)
	for _, f := range wantFilenames {
		want[f] = true
	}

	gotMap := make(map[string]bool)
	for _, f := range got {
		base := filepath.Base(f.Path)
		gotMap[base] = true
		if f.Agent != wantAgent {
			t.Errorf("file %q: agent = %q, want %q", base, f.Agent, wantAgent)
		}
	}

	if len(got) != len(want) {
		t.Errorf("got %d files total, want %d", len(got), len(want))
	}

	for file := range want {
		if !gotMap[file] {
			t.Errorf("missing expected file: %q", file)
		}
	}

	// Check for unexpected files
	for file := range gotMap {
		if !want[file] {
			t.Errorf("got unexpected file: %q", file)
		}
	}
}

func TestDiscoverClaudeProjects(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		wantFiles []string
	}{
		{
			name: "Basic",
			files: map[string]string{
				filepath.Join("project-a", "abc.jsonl"):       "{}",
				filepath.Join("project-a", "def.jsonl"):       "{}",
				filepath.Join("project-a", "agent-123.jsonl"): "{}", // Should be ignored
				filepath.Join("project-b", "xyz.jsonl"):       "{}",
			},
			wantFiles: []string{
				"abc.jsonl",
				"def.jsonl",
				"xyz.jsonl",
			},
		},
		{
			name: "Subagents",
			files: map[string]string{
				filepath.Join("project-a", "parent-session.jsonl"):                           "{}",
				filepath.Join("project-a", "parent-session", "subagents", "agent-abc.jsonl"): "{}",
				filepath.Join("project-a", "parent-session", "subagents", "agent-def.jsonl"): "{}",
				filepath.Join("project-a", "parent-session", "subagents", "not-agent.jsonl"): "{}",
			},
			wantFiles: []string{
				"parent-session.jsonl",
				"agent-abc.jsonl",
				"agent-def.jsonl",
			},
		},
		{
			name:      "Empty",
			files:     map[string]string{},
			wantFiles: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFileSystem(t, dir, tt.files)
			files := DiscoverClaudeProjects(dir)

			assertDiscoveredFiles(t, files, tt.wantFiles, AgentClaude)

			if tt.name == "Subagents" {
				for _, f := range files {
					if f.Project != "project-a" {
						t.Errorf("file %q: project = %q, want %q",
							filepath.Base(f.Path), f.Project, "project-a")
					}
				}
			}
		})
	}

	t.Run("Nonexistent", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "does-not-exist")
		files := DiscoverClaudeProjects(dir)
		if files != nil {
			t.Errorf("expected nil, got %d files", len(files))
		}
	})
}

func TestDiscoverCodexSessions(t *testing.T) {
	file1 := "rollout-123-abc-def-ghi-jkl-mno.jsonl"
	file2 := "rollout-456-abc-def-ghi-jkl-mno.jsonl"

	tests := []struct {
		name      string
		files     map[string]string
		wantFiles []string
	}{
		{
			name: "Basic",
			files: map[string]string{
				filepath.Join("2024", "01", "15", file1): "{}",
				filepath.Join("2024", "02", "01", file2): "{}",
			},
			wantFiles: []string{file1, file2},
		},
		{
			name: "SkipsNonDigit",
			files: map[string]string{
				filepath.Join("notes", "01", "01", "x.jsonl"): "{}",
			},
			wantFiles: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFileSystem(t, dir, tt.files)
			files := DiscoverCodexSessions(dir)
			assertDiscoveredFiles(t, files, tt.wantFiles, AgentCodex)
		})
	}
}

func TestDiscoverAmpSessions(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		wantFiles []string
	}{
		{
			name: "Basic",
			files: map[string]string{
				"T-019ca26f-aaaa-bbbb-cccc-dddddddddddd.json": "{}",
				"T-019ca26f-ffff-eeee-dddd-cccccccccccc.json": "{}",
				"T-.json":         "{}",
				"T--invalid.json": "{}",
				"README.md":       "{}",
				"T-not-json.txt":  "{}",
			},
			wantFiles: []string{
				"T-019ca26f-aaaa-bbbb-cccc-dddddddddddd.json",
				"T-019ca26f-ffff-eeee-dddd-cccccccccccc.json",
			},
		},
		{
			name:      "Empty",
			files:     map[string]string{},
			wantFiles: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFileSystem(t, dir, tt.files)
			files := DiscoverAmpSessions(dir)
			assertDiscoveredFiles(
				t, files, tt.wantFiles, AgentAmp,
			)
		})
	}

	t.Run("Nonexistent", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "does-not-exist")
		files := DiscoverAmpSessions(dir)
		if files != nil {
			t.Errorf("expected nil, got %d files", len(files))
		}
	})
}

func TestFindClaudeSourceFile(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		targetID string
		wantFile string
	}{
		{
			name: "Found",
			files: map[string]string{
				filepath.Join("project-a", "session-abc.jsonl"): "{}",
			},
			targetID: "session-abc",
			wantFile: filepath.Join("project-a", "session-abc.jsonl"),
		},
		{
			name: "Subagent",
			files: map[string]string{
				filepath.Join("project-a", "parent-sess", "subagents", "agent-sub1.jsonl"): "{}",
			},
			targetID: "agent-sub1",
			wantFile: filepath.Join("project-a", "parent-sess", "subagents", "agent-sub1.jsonl"),
		},
		{
			name: "Nonexistent",
			files: map[string]string{
				filepath.Join("project-a", "session-abc.jsonl"): "{}",
			},
			targetID: "nonexistent",
			wantFile: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFileSystem(t, dir, tt.files)

			got := FindClaudeSourceFile(dir, tt.targetID)
			want := ""
			if tt.wantFile != "" {
				want = filepath.Join(dir, tt.wantFile)
			}

			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}

	t.Run("Validation", func(t *testing.T) {
		dir := t.TempDir()
		tests := []string{"", "../etc/passwd", "a/b", "a b"}
		for _, id := range tests {
			got := FindClaudeSourceFile(dir, id)
			if got != "" {
				t.Errorf("FindClaudeSourceFile(%q) = %q, want empty", id, got)
			}
		}
	})
}

func TestFindAmpSourceFile(t *testing.T) {
	t.Run("Found", func(t *testing.T) {
		dir := t.TempDir()
		rel := "T-019ca26f-aaaa-bbbb-cccc-dddddddddddd.json"
		setupFileSystem(t, dir, map[string]string{
			rel: "{}",
		})
		got := FindAmpSourceFile(
			dir, "T-019ca26f-aaaa-bbbb-cccc-dddddddddddd",
		)
		want := filepath.Join(dir, rel)
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("Nonexistent", func(t *testing.T) {
		dir := t.TempDir()
		got := FindAmpSourceFile(
			dir, "T-019ca26f-aaaa-bbbb-cccc-dddddddddddd",
		)
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("Validation", func(t *testing.T) {
		dir := t.TempDir()
		tests := []string{
			"",
			"../bad",
			"T bad",
			"bad",
			"T-",
		}
		for _, id := range tests {
			got := FindAmpSourceFile(dir, id)
			if got != "" {
				t.Errorf(
					"FindAmpSourceFile(%q) = %q, want empty",
					id, got,
				)
			}
		}
	})
}

func TestFindCodexSourceFile(t *testing.T) {
	uuid := "abc12345-1234-5678-9abc-def012345678"
	filename := "rollout-20240115-" + uuid + ".jsonl"
	relPath := filepath.Join("2024", "01", "15", filename)

	tests := []struct {
		name     string
		files    map[string]string
		targetID string
		wantFile string
	}{
		{
			name:     "Found",
			files:    map[string]string{relPath: "{}"},
			targetID: uuid,
			wantFile: relPath,
		},
		{
			name:     "Nonexistent",
			files:    map[string]string{relPath: "{}"},
			targetID: "nonexistent-uuid",
			wantFile: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFileSystem(t, dir, tt.files)

			got := FindCodexSourceFile(dir, tt.targetID)
			want := ""
			if tt.wantFile != "" {
				want = filepath.Join(dir, tt.wantFile)
			}

			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestExtractUUIDFromRollout(t *testing.T) {
	tests := []struct {
		filename string
		want     string
	}{
		{
			"rollout-20240115-abc12345-1234-5678-9abc-def012345678.jsonl",
			"abc12345-1234-5678-9abc-def012345678",
		},
		{
			"rollout-20240115T100000-abc12345-1234-5678-9abc-def012345678.jsonl",
			"abc12345-1234-5678-9abc-def012345678",
		},
		{
			"short.jsonl",
			"",
		},
		{
			"rollout-20240115-12345678-1234-1234-1234-1234567890ab-abc12345-1234-5678-9abc-def012345678.jsonl",
			"abc12345-1234-5678-9abc-def012345678",
		},
		{
			"rollout-20240115-abc12345-1234-5678-9abc-def012345678-suffix.jsonl",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			got := extractUUIDFromRollout(tt.filename)
			if got != tt.want {
				t.Errorf("extractUUID(%q) = %q, want %q",
					tt.filename, got, tt.want)
			}
		})
	}
}

func TestIsValidSessionID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"abc-123", true},
		{"session_1", true},
		{"abc123", true},
		{"", false},
		{"../etc", false},
		{"a b", false},
		{"a/b", false},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got := IsValidSessionID(tt.id)
			if got != tt.want {
				t.Errorf("IsValidSessionID(%q) = %v, want %v",
					tt.id, got, tt.want)
			}
		})
	}
}

func TestIsDigits(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"123", true},
		{"0", true},
		{"", false},
		{"12a", false},
		{"abc", false},
		{"\uff11\uff12\uff13", true}, // Fullwidth digits are supported
	}

	for _, tt := range tests {
		t.Run(tt.s, func(t *testing.T) {
			got := IsDigits(tt.s)
			if got != tt.want {
				t.Errorf("IsDigits(%q) = %v, want %v",
					tt.s, got, tt.want)
			}
		})
	}
}

func TestDiscoverGeminiSessions(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		wantFiles []string
	}{
		{
			name: "Basic",
			files: map[string]string{
				filepath.Join("tmp", "hash1", geminiChatsDir, "session-2026-01-01T10-00-abc123.json"): "{}",
				filepath.Join("tmp", "hash1", geminiChatsDir, "session-2026-01-02T10-00-def456.json"): "{}",
				filepath.Join("tmp", "hash2", geminiChatsDir, "session-2026-01-03T10-00-ghi789.json"): "{}",
			},
			wantFiles: []string{
				filepath.Join("tmp", "hash1", geminiChatsDir, "session-2026-01-01T10-00-abc123.json"),
				filepath.Join("tmp", "hash1", geminiChatsDir, "session-2026-01-02T10-00-def456.json"),
				filepath.Join("tmp", "hash2", geminiChatsDir, "session-2026-01-03T10-00-ghi789.json"),
			},
		},
		{
			name: "NoChatDir",
			files: map[string]string{
				filepath.Join("tmp", "hash1", "other.txt"): "{}",
			},
			wantFiles: nil,
		},
		{
			name: "SkipsNonSessionFiles",
			files: map[string]string{
				filepath.Join("tmp", "hash1", geminiChatsDir, "session-abc.json"): "{}",
				filepath.Join("tmp", "hash1", geminiChatsDir, "other.json"):       "{}",
				filepath.Join("tmp", "hash1", geminiChatsDir, "session-def.txt"):  "{}",
			},
			wantFiles: []string{
				filepath.Join("tmp", "hash1", geminiChatsDir, "session-abc.json"),
			},
		},
		{
			name: "NamedDirs",
			files: map[string]string{
				filepath.Join("tmp", "my-project", geminiChatsDir, "session-2026-01-01T10-00-abc.json"): "{}",
			},
			wantFiles: []string{
				filepath.Join("tmp", "my-project", geminiChatsDir, "session-2026-01-01T10-00-abc.json"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFileSystem(t, dir, tt.files)

			files := DiscoverGeminiSessions(dir)

			if len(files) != len(tt.wantFiles) {
				t.Fatalf("got %d files, want %d", len(files), len(tt.wantFiles))
			}

			wantMap := make(map[string]bool)
			for _, p := range tt.wantFiles {
				wantMap[filepath.Join(dir, p)] = true
			}

			for _, f := range files {
				if f.Agent != AgentGemini {
					t.Errorf("agent = %q, want %q", f.Agent, AgentGemini)
				}
				if !wantMap[f.Path] {
					t.Errorf("unexpected file discovered: %q", f.Path)
				}
			}
		})
	}

	t.Run("EmptyChatDir", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "tmp", "hash1", geminiChatsDir), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		files := DiscoverGeminiSessions(dir)
		if files != nil {
			t.Errorf("expected nil, got %d files", len(files))
		}
	})

	t.Run("Nonexistent", func(t *testing.T) {
		files := DiscoverGeminiSessions(filepath.Join(t.TempDir(), "does-not-exist"))
		if files != nil {
			t.Errorf("expected nil, got %d files", len(files))
		}
	})

	t.Run("EmptyDir", func(t *testing.T) {
		files := DiscoverGeminiSessions("")
		if files != nil {
			t.Errorf("expected nil, got %d files", len(files))
		}
	})
}

func TestFindGeminiSourceFile(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		targetID string
		wantFile string
	}{
		{
			name: "Found",
			files: map[string]string{
				filepath.Join("tmp", "hash1", geminiChatsDir, "session-2026-01-19T18-21-b0a4eadd.json"): `{"sessionId":"b0a4eadd-cb99-4165-94d9-64cad5a66d24","messages":[]}`,
			},
			targetID: "b0a4eadd-cb99-4165-94d9-64cad5a66d24",
			wantFile: filepath.Join("tmp", "hash1", geminiChatsDir, "session-2026-01-19T18-21-b0a4eadd.json"),
		},
		{
			name: "Nonexistent",
			files: map[string]string{
				filepath.Join("tmp", "hash1", geminiChatsDir, "session-2026-01-19T18-21-b0a4eadd.json"): `{"sessionId":"b0a4eadd-cb99-4165-94d9-64cad5a66d24","messages":[]}`,
			},
			targetID: "nonexistent-uuid-1234",
			wantFile: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFileSystem(t, dir, tt.files)

			got := FindGeminiSourceFile(dir, tt.targetID)
			want := ""
			if tt.wantFile != "" {
				want = filepath.Join(dir, tt.wantFile)
			}

			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}

	t.Run("ShortID", func(t *testing.T) {
		dir := t.TempDir()
		for _, id := range []string{"", "a", "abc", "1234567"} {
			got := FindGeminiSourceFile(dir, id)
			if got != "" {
				t.Errorf("FindGeminiSourceFile(%q) = %q, want empty", id, got)
			}
		}
	})

	t.Run("EmptyDir", func(t *testing.T) {
		got := FindGeminiSourceFile("", "b0a4eadd-cb99-4165-94d9-64cad5a66d24")
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
}

func TestGeminiPathHash(t *testing.T) {
	hash := geminiPathHash("/Users/alice/code/sample-repo")
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64", len(hash))
	}
	if geminiPathHash("/Users/alice/code/sample-repo") != hash {
		t.Error("hash not deterministic")
	}
}

func TestBuildGeminiProjectMap(t *testing.T) {
	dir := t.TempDir()
	projectsJSON := `{"projects":{"/Users/alice/code/my-app":"my-app"}}`
	if err := os.WriteFile(
		filepath.Join(dir, "projects.json"),
		[]byte(projectsJSON), 0o644,
	); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := BuildGeminiProjectMap(dir)

	hash := geminiPathHash("/Users/alice/code/my-app")
	if m[hash] != "my_app" {
		t.Errorf("project for hash = %q, want %q",
			m[hash], "my_app")
	}

	if m["my-app"] != "my_app" {
		t.Errorf("project for name = %q, want %q",
			m["my-app"], "my_app")
	}
}

func TestBuildGeminiProjectMapMissingFile(t *testing.T) {
	m := BuildGeminiProjectMap(t.TempDir())
	if len(m) != 0 {
		t.Errorf("expected empty map, got %d entries", len(m))
	}
}

func TestResolveGeminiProject(t *testing.T) {
	projectMap := map[string]string{
		geminiPathHash("/Users/alice/code/my-app"): "my_app",
		"my-app":    "my_app",
		"worktree1": "main_repo",
	}

	tests := []struct {
		name    string
		dirName string
		want    string
	}{
		{
			"HashLookupHit",
			geminiPathHash("/Users/alice/code/my-app"),
			"my_app",
		},
		{
			"HashLookupMiss",
			geminiPathHash("/Users/alice/code/other"),
			"unknown",
		},
		{
			"NamedDirInMap",
			"my-app",
			"my_app",
		},
		{
			"NamedDirWorktreeResolved",
			"worktree1",
			"main_repo",
		},
		{
			"NamedDirNotInMap",
			"new-project",
			"new_project",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveGeminiProject(
				tt.dirName, projectMap,
			)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildGeminiProjectMapTrustedFolders(t *testing.T) {
	dir := t.TempDir()

	tfJSON := `{"trustedFolders":["/Users/alice/code/my-app","/Users/alice/code/other"]}`
	if err := os.WriteFile(
		filepath.Join(dir, "trustedFolders.json"),
		[]byte(tfJSON), 0o644,
	); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := BuildGeminiProjectMap(dir)

	hash1 := geminiPathHash("/Users/alice/code/my-app")
	if m[hash1] != "my_app" {
		t.Errorf("hash for my-app = %q, want %q",
			m[hash1], "my_app")
	}
	hash2 := geminiPathHash("/Users/alice/code/other")
	if m[hash2] != "other" {
		t.Errorf("hash for other = %q, want %q",
			m[hash2], "other")
	}
}

func TestBuildGeminiProjectMapBothFiles(t *testing.T) {
	dir := t.TempDir()

	pJSON := `{"projects":{"/Users/alice/code/proj-a":"proj-a"}}`
	if err := os.WriteFile(
		filepath.Join(dir, "projects.json"),
		[]byte(pJSON), 0o644,
	); err != nil {
		t.Fatalf("write: %v", err)
	}

	tfJSON := `{"trustedFolders":["/Users/alice/code/proj-b"]}`
	if err := os.WriteFile(
		filepath.Join(dir, "trustedFolders.json"),
		[]byte(tfJSON), 0o644,
	); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := BuildGeminiProjectMap(dir)

	hashA := geminiPathHash("/Users/alice/code/proj-a")
	if m[hashA] != "proj_a" {
		t.Errorf("proj-a hash = %q, want %q",
			m[hashA], "proj_a")
	}
	hashB := geminiPathHash("/Users/alice/code/proj-b")
	if m[hashB] != "proj_b" {
		t.Errorf("proj-b hash = %q, want %q",
			m[hashB], "proj_b")
	}
}

func TestBuildGeminiProjectMapProjectsWin(t *testing.T) {
	dir := t.TempDir()

	pJSON := `{"projects":{"/Users/alice/code/my-app":"my-app"}}`
	if err := os.WriteFile(
		filepath.Join(dir, "projects.json"),
		[]byte(pJSON), 0o644,
	); err != nil {
		t.Fatalf("write: %v", err)
	}

	tfJSON := `{"trustedFolders":["/Users/alice/code/my-app"]}`
	if err := os.WriteFile(
		filepath.Join(dir, "trustedFolders.json"),
		[]byte(tfJSON), 0o644,
	); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := BuildGeminiProjectMap(dir)

	hash := geminiPathHash("/Users/alice/code/my-app")
	if m[hash] != "my_app" {
		t.Errorf("hash = %q, want %q", m[hash], "my_app")
	}
	if m["my-app"] != "my_app" {
		t.Errorf("name key = %q, want %q",
			m["my-app"], "my_app")
	}
}

// --- Copilot discovery tests ---

func TestDiscoverCopilotSessions(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		wantFiles []string
	}{
		{
			name: "BareFormat",
			files: map[string]string{
				filepath.Join(copilotStateDir, "abc-123.jsonl"): "{}",
				filepath.Join(copilotStateDir, "def-456.jsonl"): "{}",
			},
			wantFiles: []string{
				filepath.Join(copilotStateDir, "abc-123.jsonl"),
				filepath.Join(copilotStateDir, "def-456.jsonl"),
			},
		},
		{
			name: "DirFormat",
			files: map[string]string{
				filepath.Join(copilotStateDir, "sess-1", "events.jsonl"): "{}",
				filepath.Join(copilotStateDir, "sess-2", "events.jsonl"): "{}",
			},
			wantFiles: []string{
				filepath.Join(copilotStateDir, "sess-1", "events.jsonl"),
				filepath.Join(copilotStateDir, "sess-2", "events.jsonl"),
			},
		},
		{
			name: "Mixed",
			files: map[string]string{
				filepath.Join(copilotStateDir, "bare-1.jsonl"):          "{}",
				filepath.Join(copilotStateDir, "dir-1", "events.jsonl"): "{}",
			},
			wantFiles: []string{
				filepath.Join(copilotStateDir, "bare-1.jsonl"),
				filepath.Join(copilotStateDir, "dir-1", "events.jsonl"),
			},
		},
		{
			name: "BareWithInvalidDir",
			files: map[string]string{
				filepath.Join(copilotStateDir, "invalid-dir-uuid.jsonl"):        "{}",
				filepath.Join(copilotStateDir, "invalid-dir-uuid", "other.txt"): "{}",
			},
			wantFiles: []string{
				filepath.Join(copilotStateDir, "invalid-dir-uuid.jsonl"),
			},
		},
		{
			name: "DedupBareAndDir",
			files: map[string]string{
				filepath.Join(copilotStateDir, "dup-uuid-1234.jsonl"):           "{}",
				filepath.Join(copilotStateDir, "dup-uuid-1234", "events.jsonl"): "{}",
			},
			wantFiles: []string{
				filepath.Join(copilotStateDir, "dup-uuid-1234", "events.jsonl"),
			},
		},
		{
			name: "DirWithoutEvents",
			files: map[string]string{
				filepath.Join(copilotStateDir, "no-events", "other.txt"): "{}",
			},
			wantFiles: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFileSystem(t, dir, tt.files)

			files := DiscoverCopilotSessions(dir)

			if len(files) != len(tt.wantFiles) {
				t.Fatalf("got %d files, want %d", len(files), len(tt.wantFiles))
			}

			wantMap := make(map[string]bool)
			for _, p := range tt.wantFiles {
				wantMap[filepath.Join(dir, p)] = true
			}

			for _, f := range files {
				if f.Agent != AgentCopilot {
					t.Errorf("agent = %q, want %q", f.Agent, AgentCopilot)
				}
				if !wantMap[f.Path] {
					t.Errorf("unexpected file discovered: %q", f.Path)
				}
			}
		})
	}

	t.Run("EmptyDir", func(t *testing.T) {
		files := DiscoverCopilotSessions("")
		if files != nil {
			t.Errorf("expected nil, got %d files", len(files))
		}
	})

	t.Run("Nonexistent", func(t *testing.T) {
		files := DiscoverCopilotSessions(filepath.Join(t.TempDir(), "does-not-exist"))
		if files != nil {
			t.Errorf("expected nil, got %d files", len(files))
		}
	})
}

func TestFindCopilotSourceFile(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		targetID string
		wantFile string
	}{
		{
			name:     "Bare",
			files:    map[string]string{filepath.Join(copilotStateDir, "abc-123.jsonl"): "{}"},
			targetID: "abc-123",
			wantFile: filepath.Join(copilotStateDir, "abc-123.jsonl"),
		},
		{
			name:     "DirFormat",
			files:    map[string]string{filepath.Join(copilotStateDir, "sess-42", "events.jsonl"): "{}"},
			targetID: "sess-42",
			wantFile: filepath.Join(copilotStateDir, "sess-42", "events.jsonl"),
		},
		{
			name:     "Nonexistent",
			files:    map[string]string{filepath.Join(copilotStateDir, "abc-123.jsonl"): "{}"},
			targetID: "nonexistent",
			wantFile: "",
		},
		{
			name: "DirPreferred",
			files: map[string]string{
				filepath.Join(copilotStateDir, "dual-1.jsonl"):           "{}",
				filepath.Join(copilotStateDir, "dual-1", "events.jsonl"): "{}",
			},
			targetID: "dual-1",
			wantFile: filepath.Join(copilotStateDir, "dual-1", "events.jsonl"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFileSystem(t, dir, tt.files)

			got := FindCopilotSourceFile(dir, tt.targetID)
			want := ""
			if tt.wantFile != "" {
				want = filepath.Join(dir, tt.wantFile)
			}

			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}

	t.Run("InvalidID", func(t *testing.T) {
		dir := t.TempDir()
		for _, id := range []string{"", "../etc/passwd", "a/b", "a b"} {
			got := FindCopilotSourceFile(dir, id)
			if got != "" {
				t.Errorf("FindCopilotSourceFile(%q) = %q, want empty", id, got)
			}
		}
	})

	t.Run("EmptyDir", func(t *testing.T) {
		got := FindCopilotSourceFile("", "abc-123")
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
}

// --- Symlink tests ---

func TestIsDirOrSymlink(t *testing.T) {
	dir := t.TempDir()

	realDir := filepath.Join(dir, "real-dir")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	realFile := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(
		realFile, []byte("hi"), 0o644,
	); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := os.Symlink(
		realDir, filepath.Join(dir, "link-to-dir"),
	); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	if err := os.Symlink(
		realFile, filepath.Join(dir, "link-to-file"),
	); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := os.Symlink(
		filepath.Join(dir, "gone"),
		filepath.Join(dir, "broken"),
	); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	want := map[string]bool{
		"real-dir":     true,
		"file.txt":     false,
		"link-to-dir":  true,
		"link-to-file": false,
		"broken":       false,
	}

	for _, e := range entries {
		expected, ok := want[e.Name()]
		if !ok {
			continue
		}
		got := isDirOrSymlink(e, dir)
		if got != expected {
			t.Errorf("isDirOrSymlink(%q) = %v, want %v",
				e.Name(), got, expected)
		}
	}
}

func TestFindClaudeSourceFile_Symlink(t *testing.T) {
	externalDir := t.TempDir()
	realDir := filepath.Join(externalDir, "real-project")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(realDir, "sess-abc.jsonl"),
		[]byte("{}"), 0o644,
	); err != nil {
		t.Fatalf("write: %v", err)
	}

	searchDir := t.TempDir()
	linkDir := filepath.Join(searchDir, "linked-project")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	got := FindClaudeSourceFile(searchDir, "sess-abc")
	if got == "" {
		t.Fatal("expected to find session via symlink")
	}
	if filepath.Dir(got) != linkDir {
		t.Errorf("expected path through symlink, got %q", got)
	}
}

func TestIsContainedIn(t *testing.T) {
	tests := []struct {
		name        string
		child, root string
		want        bool
	}{
		{
			name:  "child under root",
			child: "/a/b/c",
			root:  "/a/b",
			want:  true,
		},
		{
			name:  "same path",
			child: "/a/b",
			root:  "/a/b",
			want:  false,
		},
		{
			name:  "child outside root",
			child: "/a/x",
			root:  "/a/b",
			want:  false,
		},
		{
			name:  "traversal",
			child: "/a/b/../x",
			root:  "/a/b",
			want:  false,
		},
		{
			name:  "parent of root",
			child: "/a",
			root:  "/a/b",
			want:  false,
		},
		{
			name:  "dotdot-prefixed name",
			child: "/a/b/..hidden",
			root:  "/a/b",
			want:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isContainedIn(tt.child, tt.root)
			if got != tt.want {
				t.Errorf(
					"isContainedIn(%q, %q) = %v, want %v",
					tt.child, tt.root, got, tt.want,
				)
			}
		})
	}
}

func TestDiscoverCursorSessions(t *testing.T) {
	cursorTranscripts := filepath.Join(
		"proj-dir", "agent-transcripts",
	)

	tests := []struct {
		name      string
		files     map[string]string
		wantCount int
	}{
		{
			name: "TxtOnly",
			files: map[string]string{
				filepath.Join(cursorTranscripts, "aaa.txt"): "user:\nhi",
			},
			wantCount: 1,
		},
		{
			name: "JsonlOnly",
			files: map[string]string{
				filepath.Join(cursorTranscripts, "bbb.jsonl"): `{"role":"user"}`,
			},
			wantCount: 1,
		},
		{
			name: "BothExtensionsDedupToJsonl",
			files: map[string]string{
				filepath.Join(cursorTranscripts, "ccc.txt"):   "user:\nhi",
				filepath.Join(cursorTranscripts, "ccc.jsonl"): `{"role":"user"}`,
			},
			wantCount: 1,
		},
		{
			name: "IgnoresOtherExtensions",
			files: map[string]string{
				filepath.Join(cursorTranscripts, "ddd.json"): "{}",
				filepath.Join(cursorTranscripts, "eee.log"):  "log",
			},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFileSystem(t, dir, tt.files)
			files := DiscoverCursorSessions(dir)
			if len(files) != tt.wantCount {
				t.Fatalf(
					"got %d files, want %d",
					len(files), tt.wantCount,
				)
			}
			for _, f := range files {
				if f.Agent != AgentCursor {
					t.Errorf(
						"agent = %q, want %q",
						f.Agent, AgentCursor,
					)
				}
			}
		})
	}
}

func TestDiscoverCursorSessions_DedupPrefersJsonl(t *testing.T) {
	dir := t.TempDir()
	transcripts := filepath.Join(
		"proj-dir", "agent-transcripts",
	)
	setupFileSystem(t, dir, map[string]string{
		filepath.Join(transcripts, "sess.txt"):   "user:\nhi",
		filepath.Join(transcripts, "sess.jsonl"): `{"role":"user"}`,
	})
	files := DiscoverCursorSessions(dir)
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if !strings.HasSuffix(files[0].Path, ".jsonl") {
		t.Errorf(
			"expected .jsonl path, got %q", files[0].Path,
		)
	}
}

func TestFindCursorSourceFile(t *testing.T) {
	cursorTranscripts := filepath.Join(
		"proj-dir", "agent-transcripts",
	)

	t.Run("FindsTxt", func(t *testing.T) {
		dir := t.TempDir()
		setupFileSystem(t, dir, map[string]string{
			filepath.Join(cursorTranscripts, "sess1.txt"): "data",
		})
		got := FindCursorSourceFile(dir, "sess1")
		if got == "" {
			t.Fatal("expected to find .txt file")
		}
	})

	t.Run("FindsJsonl", func(t *testing.T) {
		dir := t.TempDir()
		setupFileSystem(t, dir, map[string]string{
			filepath.Join(cursorTranscripts, "sess2.jsonl"): "{}",
		})
		got := FindCursorSourceFile(dir, "sess2")
		if got == "" {
			t.Fatal("expected to find .jsonl file")
		}
	})

	t.Run("PrefersJsonlWhenBothExist", func(t *testing.T) {
		dir := t.TempDir()
		setupFileSystem(t, dir, map[string]string{
			filepath.Join(cursorTranscripts, "sess3.txt"):   "old",
			filepath.Join(cursorTranscripts, "sess3.jsonl"): "new",
		})
		jsonlPath := filepath.Join(
			dir, cursorTranscripts, "sess3.jsonl",
		)
		got := FindCursorSourceFile(dir, "sess3")
		if got != jsonlPath {
			t.Errorf(
				"got %q, want %q (.jsonl preferred)",
				got, jsonlPath,
			)
		}
	})

	t.Run("NotFound", func(t *testing.T) {
		dir := t.TempDir()
		got := FindCursorSourceFile(dir, "nonexistent")
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
}

func TestIsPiSessionFile(t *testing.T) {
	t.Run("ValidSession", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "pi-*.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(`{"type":"session","id":"abc"}` + "\n")
		f.Close()
		if !isPiSessionFile(f.Name()) {
			t.Error("expected true for valid session file")
		}
	})

	t.Run("LongHeaderLine", func(t *testing.T) {
		// Header line longer than 1 MiB — the old 1 MiB buffer would fail.
		padding := strings.Repeat("x", 2*1024*1024)
		line := `{"type":"session","id":"abc","pad":"` + padding + `"}` + "\n"
		f, err := os.CreateTemp(t.TempDir(), "pi-*.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(line)
		f.Close()
		if !isPiSessionFile(f.Name()) {
			t.Error("expected true for session file with long header line (>1 MiB)")
		}
	})

	t.Run("LeadingBlankLines", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "pi-*.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString("\n\n" + `{"type":"session","id":"abc"}` + "\n")
		f.Close()
		if !isPiSessionFile(f.Name()) {
			t.Error("expected true for session file with leading blank lines")
		}
	})

	t.Run("NonSessionJSON", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "pi-*.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(`{"type":"message","id":"abc"}` + "\n")
		f.Close()
		if isPiSessionFile(f.Name()) {
			t.Error("expected false for non-session JSON")
		}
	})

	t.Run("EmptyFile", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "pi-*.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		if isPiSessionFile(f.Name()) {
			t.Error("expected false for empty file")
		}
	})
}
