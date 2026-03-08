package sync_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/dbtest"
	"github.com/wesm/agentsview/internal/parser"
	"github.com/wesm/agentsview/internal/sync"
	"github.com/wesm/agentsview/internal/testjsonl"
)

type testEnv struct {
	claudeDir   string
	codexDir    string
	cursorDir   string
	geminiDir   string
	opencodeDir string
	ampDir      string
	piDir       string
	db          *db.DB
	engine      *sync.Engine
}

type testEnvOpts struct {
	claudeDirs []string
	codexDirs  []string
	cursorDirs []string
}

type TestEnvOption func(*testEnvOpts)

func WithClaudeDirs(dirs []string) TestEnvOption {
	return func(o *testEnvOpts) {
		o.claudeDirs = dirs
	}
}

func WithCodexDirs(dirs []string) TestEnvOption {
	return func(o *testEnvOpts) {
		o.codexDirs = dirs
	}
}

func WithCursorDirs(dirs []string) TestEnvOption {
	return func(o *testEnvOpts) {
		o.cursorDirs = dirs
	}
}

func setupTestEnv(t *testing.T, opts ...TestEnvOption) *testEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	options := testEnvOpts{}
	for _, opt := range opts {
		opt(&options)
	}

	env := &testEnv{
		geminiDir:   t.TempDir(),
		opencodeDir: t.TempDir(),
		ampDir:      t.TempDir(),
		piDir:       t.TempDir(),
		db:          dbtest.OpenTestDB(t),
	}

	claudeDirs := options.claudeDirs
	if len(claudeDirs) == 0 {
		env.claudeDir = t.TempDir()
		claudeDirs = []string{env.claudeDir}
	} else {
		env.claudeDir = claudeDirs[0]
	}

	codexDirs := options.codexDirs
	if len(codexDirs) == 0 {
		env.codexDir = t.TempDir()
		codexDirs = []string{env.codexDir}
	} else {
		env.codexDir = codexDirs[0]
	}

	cursorDirs := options.cursorDirs
	if len(cursorDirs) == 0 {
		env.cursorDir = t.TempDir()
		cursorDirs = []string{env.cursorDir}
	} else {
		env.cursorDir = cursorDirs[0]
	}

	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude:   claudeDirs,
			parser.AgentCodex:    codexDirs,
			parser.AgentCursor:   cursorDirs,
			parser.AgentGemini:   {env.geminiDir},
			parser.AgentOpenCode: {env.opencodeDir},
			parser.AgentAmp:      {env.ampDir},
			parser.AgentPi:       {env.piDir},
		},
		Machine: "local",
	})
	return env
}

// writeSession creates a JSONL session file under baseDir at
// the given relative path, creating parent directories as
// needed. Returns the full file path.
func (e *testEnv) writeSession(
	t *testing.T, baseDir, relPath, content string,
) string {
	t.Helper()
	path := filepath.Join(baseDir, relPath)
	dbtest.WriteTestFile(t, path, []byte(content))
	return path
}

// writeClaudeSession creates a JSONL session file under the
// Claude projects directory.
func (e *testEnv) writeClaudeSession(
	t *testing.T, projName, filename, content string,
) string {
	t.Helper()
	return e.writeSession(
		t, e.claudeDir,
		filepath.Join(projName, filename), content,
	)
}

// writeClaudeSessionForProject creates a JSONL session file under the
// Claude projects directory using a standard un-sanitized directory path.
func (e *testEnv) writeClaudeSessionForProject(
	t *testing.T, dirPath, filename, content string,
) string {
	t.Helper()
	projName := strings.ReplaceAll(dirPath, "/", "-")
	return e.writeClaudeSession(t, projName, filename, content)
}

// writeCodexSession creates a JSONL session file under the
// Codex date-based directory.
func (e *testEnv) writeCodexSession(
	t *testing.T, dayPath, filename, content string,
) string {
	t.Helper()
	return e.writeSession(
		t, e.codexDir,
		filepath.Join(dayPath, filename), content,
	)
}

// writeGeminiSession creates a JSON session file under the
// Gemini directory at the given relative path.
func (e *testEnv) writeGeminiSession(
	t *testing.T, relPath, content string,
) string {
	t.Helper()
	return e.writeSession(t, e.geminiDir, relPath, content)
}

// writeAmpThread creates an Amp thread JSON file under the
// configured Amp threads directory.
func (e *testEnv) writeAmpThread(
	t *testing.T, filename, content string,
) string {
	t.Helper()
	return e.writeSession(t, e.ampDir, filename, content)
}

// writeCursorSession creates a Cursor transcript file under
// the given cursorDir at <project>/agent-transcripts/<file>.
func (e *testEnv) writeCursorSession(
	t *testing.T, cursorDir, project, filename,
	content string,
) string {
	t.Helper()
	return e.writeSession(
		t, cursorDir,
		filepath.Join(
			project, "agent-transcripts", filename,
		),
		content,
	)
}

func TestSyncEngineIntegration(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Hello", "/Users/alice/code/my-app").
		AddClaudeAssistant(tsEarlyS5, "Hi there!").
		String()

	env.writeClaudeSessionForProject(
		t, "/Users/alice/code/my-app",
		"test-session.jsonl", content,
	)

	// First sync should parse
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1, Synced: 1, Skipped: 0})

	// Verify session was stored
	assertSessionProject(t, env.db, "test-session", "my_app")
	assertSessionMessageCount(t, env.db, "test-session", 2)

	// Verify messages
	assertMessageRoles(
		t, env.db, "test-session", "user", "assistant",
	)

	// Second sync should skip (unchanged files)
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 0 + 1, Synced: 0, Skipped: 1})

	// FindSourceFile
	src := env.engine.FindSourceFile("test-session")
	if src == "" {
		t.Error("FindSourceFile returned empty")
	}
}

func TestSyncEngineWorktreesShareProject(t *testing.T) {
	env := setupTestEnv(t)

	root := t.TempDir()
	mainRepo := filepath.Join(root, "agentsview")
	worktree := filepath.Join(root, "agentsview-worktree-tool-call-arguments")
	worktreeGitDir := filepath.Join(mainRepo, ".git", "worktrees", "feature")

	dbtest.WriteTestFile(t, filepath.Join(worktree, ".git"),
		[]byte("gitdir: "+worktreeGitDir+"\n"))
	dbtest.WriteTestFile(t, filepath.Join(worktreeGitDir, "commondir"),
		[]byte("../..\n"))

	// Create a standard main repository marker.
	if err := os.MkdirAll(filepath.Join(mainRepo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir main .git: %v", err)
	}

	mainContent := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Main repo", mainRepo).
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()
	worktreeContent := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Worktree", worktree).
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()

	env.writeClaudeSessionForProject(
		t, "/Users/me/code/agentsview",
		"main-repo.jsonl", mainContent,
	)
	env.writeClaudeSessionForProject(
		t, "/Users/me/code/agentsview-worktree-tool-call-arguments",
		"worktree-repo.jsonl", worktreeContent,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 2 + 0, Synced: 2, Skipped: 0})

	assertSessionProject(t, env.db, "main-repo", "agentsview")
	assertSessionProject(t, env.db, "worktree-repo", "agentsview")

	projects, err := env.db.GetProjects(context.Background())
	if err != nil {
		t.Fatalf("GetProjects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("len(projects) = %d, want 1", len(projects))
	}
	if projects[0].Name != "agentsview" {
		t.Fatalf("project name = %q, want %q", projects[0].Name, "agentsview")
	}
	if projects[0].SessionCount != 2 {
		t.Fatalf("session_count = %d, want 2", projects[0].SessionCount)
	}
}

func TestSyncEngineWorktreeProjectWhenPathMissing(t *testing.T) {
	env := setupTestEnv(t)

	mainContent := testjsonl.NewSessionBuilder().
		AddRaw(`{"type":"user","timestamp":"2024-01-01T10:00:00Z","cwd":"/Users/wesm/code/agentsview","gitBranch":"main","message":{"content":"hello"}}`).
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()

	worktreeContent := testjsonl.NewSessionBuilder().
		AddRaw(`{"type":"user","timestamp":"2024-01-01T10:00:00Z","cwd":"/Users/wesm/code/agentsview-worktree-tool-call-arguments","gitBranch":"worktree-tool-call-arguments","message":{"content":"hello"}}`).
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()

	env.writeClaudeSessionForProject(
		t, "/Users/me/code/agentsview",
		"offline-main.jsonl", mainContent,
	)
	env.writeClaudeSessionForProject(
		t, "/Users/me/code/agentsview-worktree-tool-call-arguments",
		"offline-worktree.jsonl", worktreeContent,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 2 + 0, Synced: 2, Skipped: 0})

	assertSessionProject(t, env.db, "offline-main", "agentsview")
	assertSessionProject(t, env.db, "offline-worktree", "agentsview")
}

func TestSyncEngineCodex(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, "test-uuid", "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Add tests").
		AddCodexMessage(tsEarlyS5, "assistant", "Adding test coverage.").
		String()

	env.writeCodexSession(
		t, filepath.Join("2024", "01", "15"),
		"rollout-20240115-test-uuid.jsonl", content,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1, Synced: 1, Skipped: 0})

	assertSessionProject(t, env.db, "codex:test-uuid", "api")
	assertSessionState(t, env.db, "codex:test-uuid", func(sess *db.Session) {
		if sess.Agent != "codex" {
			t.Errorf("agent = %q", sess.Agent)
		}
	})
}

func TestSyncEngineProgress(t *testing.T) {
	env := setupTestEnv(t)

	msg := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "msg").
		String()

	for _, name := range []string{"a", "b", "c"} {
		env.writeClaudeSession(
			t, "test-proj", name+".jsonl", msg,
		)
	}

	var progressCalls int
	env.engine.SyncAll(func(p sync.Progress) {
		progressCalls++
	})

	if progressCalls == 0 {
		t.Error("expected progress callbacks")
	}
}

func TestSyncEngineHashSkip(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "msg1").
		String()

	path := env.writeClaudeSession(
		t, "test-proj", "hash-test.jsonl", content,
	)

	// First sync
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1 + 0, Synced: 1, Skipped: 0})

	// Verify file metadata was stored
	size, mtime, ok := env.db.GetSessionFileInfo("hash-test")
	if !ok {
		t.Fatal("file info not stored")
	}
	if mtime == 0 {
		t.Fatal("mtime not stored")
	}
	if size == 0 {
		t.Fatal("size not stored")
	}

	// Second sync — unchanged content → skipped
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 0 + 1, Synced: 0, Skipped: 1})

	// Overwrite with different content (changes mtime).
	different := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "msg2").
		String()
	os.WriteFile(path, []byte(different), 0o644)

	// Third sync — mtime changed → re-synced
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1 + 0, Synced: 1, Skipped: 0})
}

func TestSyncEngineSkipCache(t *testing.T) {
	env := setupTestEnv(t)

	// Write malformed content that produces 0 valid messages
	path := env.writeClaudeSession(
		t, "test-proj", "skip-test.jsonl",
		"not json at all\x00\x01",
	)

	// First sync — file parsed (empty session stored)
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1, Synced: 1, Skipped: 0})

	// Second sync — unchanged mtime, should be skipped
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 0 + 1, Synced: 0, Skipped: 1})

	// Touch file (change mtime) but keep same content
	time.Sleep(10 * time.Millisecond)
	os.Chtimes(path, time.Now(), time.Now())

	// Third sync — mtime changed → re-synced (harmless)
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1 + 0, Synced: 1, Skipped: 0})
}

func TestSyncEngineFileAppend(t *testing.T) {
	env := setupTestEnv(t)

	initial := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "first").
		String()

	path := env.writeClaudeSession(
		t, "test-proj", "append-test.jsonl", initial,
	)

	// First sync
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1 + 0, Synced: 1, Skipped: 0})

	assertSessionMessageCount(t, env.db, "append-test", 1)

	// Append a new message (changes size and hash)
	appended := initial + testjsonl.NewSessionBuilder().
		AddClaudeAssistant(tsZeroS5, "reply").
		String()

	os.WriteFile(path, []byte(appended), 0o644)

	// Re-sync — different size → re-synced
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1 + 0, Synced: 1, Skipped: 0})

	assertSessionMessageCount(t, env.db, "append-test", 2)
}

// TestSyncSingleSessionReplacesContent verifies that an
// explicit SyncSingleSession replaces existing message
// content (same ordinals, different text).
func TestSyncSingleSessionReplacesContent(
	t *testing.T,
) {
	env := setupTestEnv(t)

	original := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "original question").
		AddClaudeAssistant(tsZeroS5, "original answer").
		String()

	path := env.writeClaudeSession(
		t, "test-proj", "replace-test.jsonl", original,
	)

	env.engine.SyncAll(nil)
	assertMessageContent(
		t, env.db, "replace-test",
		"original question", "original answer",
	)

	// Rewrite the file with different content but same
	// number of messages (same ordinals 0 and 1).
	updated := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "updated question").
		AddClaudeAssistant(tsZeroS5, "updated answer").
		String()
	os.WriteFile(path, []byte(updated), 0o644)

	// SyncSingleSession should fully replace messages.
	if err := env.engine.SyncSingleSession(
		"replace-test",
	); err != nil {
		t.Fatalf("SyncSingleSession: %v", err)
	}

	assertMessageContent(
		t, env.db, "replace-test",
		"updated question", "updated answer",
	)
}

func TestSyncSingleSessionHash(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "hello").
		AddClaudeAssistant(tsZeroS5, "hi").
		String()

	env.writeClaudeSession(
		t, "test-proj", "single-hash.jsonl", content,
	)

	env.engine.SyncAll(nil)
	env.assertResyncRoundTrip(t, "single-hash")
}

func TestSyncSingleSessionHashCodex(t *testing.T) {
	env := setupTestEnv(t)

	uuid := "a1b2c3d4-1234-5678-9abc-def012345678"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Add tests").
		AddCodexMessage(tsEarlyS5, "assistant", "Adding test coverage.").
		String()

	env.writeCodexSession(
		t, filepath.Join("2024", "01", "15"),
		"rollout-20240115-"+uuid+".jsonl", content,
	)

	sessionID := "codex:" + uuid

	env.engine.SyncAll(nil)
	env.assertResyncRoundTrip(t, sessionID)
}

func TestSyncSingleSessionCodexExecBypassesCache(
	t *testing.T,
) {
	env := setupTestEnv(t)

	uuid := "e5f6a7b8-5678-9012-cdef-123456789012"
	// Exec-originated session: SyncAll skips these, but
	// SyncSingleSession should still find them.
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			tsEarly, uuid,
			"/home/user/code/api", "codex_exec",
		).
		AddCodexMessage(tsEarlyS1, "user", "run ls").
		AddCodexMessage(tsEarlyS5, "assistant", "done").
		String()

	env.writeCodexSession(
		t, filepath.Join("2024", "01", "15"),
		"rollout-20240115-"+uuid+".jsonl", content,
	)

	// SyncAll skips exec-originated sessions (nil result).
	env.engine.SyncAll(nil)
	sess, _ := env.db.GetSession(
		context.Background(), "codex:"+uuid,
	)
	if sess != nil {
		t.Fatal("exec session should not appear after SyncAll")
	}

	// SyncSingleSession should bypass the skip cache and
	// parse with includeExec=true.
	err := env.engine.SyncSingleSession("codex:" + uuid)
	if err != nil {
		t.Fatalf("SyncSingleSession: %v", err)
	}

	assertSessionState(
		t, env.db, "codex:"+uuid,
		func(sess *db.Session) {
			if sess.Agent != "codex" {
				t.Errorf("agent = %q, want codex",
					sess.Agent)
			}
		},
	)
}

func TestSyncEngineTombstoneClearOnMtimeChange(t *testing.T) {
	env := setupTestEnv(t)

	// Write something that produces 0 messages but parses OK
	path := env.writeClaudeSession(
		t, "test-proj", "tombstone-clear.jsonl", "garbage\n",
	)

	// First sync
	env.engine.SyncAll(nil)

	// Replace with valid content
	valid := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "hello").
		AddClaudeAssistant(tsZeroS5, "hi").
		String()

	os.WriteFile(path, []byte(valid), 0o644)

	// Re-sync — content changed (different size) → re-synced
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1 + 0, Synced: 1, Skipped: 0})

	assertSessionMessageCount(t, env.db, "tombstone-clear", 2)
}

func TestSyncSingleSessionProjectFallback(t *testing.T) {
	env := setupTestEnv(t)

	// 1. Create a session in a directory "default-proj"
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "hello").
		String()

	env.writeClaudeSession(
		t, "default-proj", "fallback-test.jsonl", content,
	)

	// 2. Initial sync - should get "default-proj"
	env.engine.SyncAll(nil)

	assertSessionProject(t, env.db, "fallback-test", "default_proj")

	// 3. Manually update project to "custom_proj"
	// This simulates a user override we want to preserve
	env.updateSessionProject(t, "fallback-test", "custom_proj")

	assertSessionProject(t, env.db, "fallback-test", "custom_proj")

	// 4. SyncSingleSession should NOT revert to "default_proj"
	err := env.engine.SyncSingleSession("fallback-test")
	if err != nil {
		t.Fatalf("SyncSingleSession: %v", err)
	}

	assertSessionProject(t, env.db, "fallback-test", "custom_proj")

	// Case A: Empty project -> should fall back to directory
	env.updateSessionProject(t, "fallback-test", "")

	err = env.engine.SyncSingleSession("fallback-test")
	if err != nil {
		t.Fatalf("SyncSingleSession (empty): %v", err)
	}

	assertSessionProject(t, env.db, "fallback-test", "default_proj")

	// Case B: Bad project -> should fall back to directory
	env.updateSessionProject(t, "fallback-test", "_Users_alice_bad")

	err = env.engine.SyncSingleSession("fallback-test")
	if err != nil {
		t.Fatalf("SyncSingleSession (bad): %v", err)
	}

	assertSessionProject(t, env.db, "fallback-test", "default_proj")
}

func TestSyncEngineNoTrailingNewline(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Hello").
		StringNoTrailingNewline()

	env.writeClaudeSession(
		t, "test-proj", "no-newline.jsonl", content,
	)

	// Sync should succeed
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1 + 0, Synced: 1, Skipped: 0})

	assertSessionMessageCount(t, env.db, "no-newline", 1)
}

func TestSyncPathsClaude(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "Hello").
		String()

	path := env.writeClaudeSession(
		t, "test-proj", "paths-test.jsonl", content,
	)

	// Initial full sync
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1 + 0, Synced: 1, Skipped: 0})

	assertSessionMessageCount(t, env.db, "paths-test", 1)

	// Append a message (changes size and hash)
	appended := content + testjsonl.NewSessionBuilder().
		AddClaudeAssistant(tsZeroS5, "reply").
		String()
	os.WriteFile(path, []byte(appended), 0o644)

	// SyncPaths with just the changed file
	env.engine.SyncPaths([]string{path})

	assertSessionMessageCount(t, env.db, "paths-test", 2)
}

func TestSyncPathsOnlyProcessesChanged(t *testing.T) {
	env := setupTestEnv(t)

	content1 := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "msg1").
		String()
	content2 := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "msg2").
		String()

	path1 := env.writeClaudeSession(
		t, "proj", "session-1.jsonl", content1,
	)
	env.writeClaudeSession(
		t, "proj", "session-2.jsonl", content2,
	)

	// Initial full sync
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 2 + 0, Synced: 2, Skipped: 0})

	// Only modify session-1
	appended := content1 + testjsonl.NewSessionBuilder().
		AddClaudeAssistant(tsZeroS5, "reply").
		String()
	os.WriteFile(path1, []byte(appended), 0o644)

	// SyncPaths with just session-1
	env.engine.SyncPaths([]string{path1})

	// session-1 should have 2 messages
	assertSessionMessageCount(t, env.db, "session-1", 2)
	// session-2 should still have 1 message (untouched)
	assertSessionMessageCount(t, env.db, "session-2", 1)
}

func TestSyncPathsIgnoresNonSessionFiles(t *testing.T) {
	env := setupTestEnv(t)

	// SyncPaths with non-session paths: no panic, no error
	env.engine.SyncPaths([]string{
		filepath.Join(env.claudeDir, "some-dir"),
		filepath.Join(env.claudeDir, "proj", "README.md"),
		"/tmp/random-file.txt",
	})
}

func TestSyncPathsCodex(t *testing.T) {
	env := setupTestEnv(t)

	uuid := "c3d4e5f6-3456-7890-abcd-ef1234567890"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			tsEarly, uuid,
			"/home/user/code/api", "user",
		).
		AddCodexMessage(tsEarlyS1, "user", "Add tests").
		String()

	path := env.writeCodexSession(
		t, filepath.Join("2024", "01", "15"),
		"rollout-20240115-"+uuid+".jsonl", content,
	)

	// SyncPaths should process this Codex file
	env.engine.SyncPaths([]string{path})

	assertSessionState(
		t, env.db, "codex:"+uuid,
		func(sess *db.Session) {
			if sess.Agent != "codex" {
				t.Errorf("agent = %q, want codex",
					sess.Agent)
			}
		},
	)
}

func TestSyncPathsIgnoresAgentFiles(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "Hello").
		String()

	// Create an agent-* file (should be ignored)
	path := env.writeClaudeSession(
		t, "proj", "agent-abc.jsonl", content,
	)

	// SyncPaths should ignore agent-* files
	env.engine.SyncPaths([]string{path})

	// No session should exist for agent-abc
	sess, _ := env.db.GetSession(
		context.Background(), "agent-abc",
	)
	if sess != nil {
		t.Error("agent-* file should be ignored")
	}
}

func TestSyncEngineCodexNoTrailingNewline(t *testing.T) {
	env := setupTestEnv(t)

	uuid := "b2c3d4e5-2345-6789-0abc-def123456789"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Hello").
		StringNoTrailingNewline()

	env.writeCodexSession(
		t, filepath.Join("2024", "01", "15"),
		"rollout-20240115-"+uuid+".jsonl", content,
	)

	// Sync should succeed
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1 + 0, Synced: 1, Skipped: 0})

	assertSessionMessageCount(t, env.db, "codex:"+uuid, 1)
}

func TestSyncPathsTrailingSlashDirs(t *testing.T) {
	// Dirs with trailing slashes should still work after
	// filepath.Clean normalisation in isUnder.
	claudeDir := t.TempDir() + "/"
	codexDir := t.TempDir() + "/"
	env := setupTestEnv(t, WithClaudeDirs([]string{claudeDir}), WithCodexDirs([]string{codexDir}))

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "Hello").
		String()

	claudePath := filepath.Join(
		claudeDir, "proj", "trailing.jsonl",
	)
	dbtest.WriteTestFile(t, claudePath, []byte(content))

	env.engine.SyncPaths([]string{claudePath})

	assertSessionMessageCount(t, env.db, "trailing", 1)
}

func TestSyncPathsGemini(t *testing.T) {
	env := setupTestEnv(t)

	sessionID := "gem-test-uuid"
	hash := "abcdef1234567890"
	content := testjsonl.GeminiSessionJSON(
		sessionID, hash, tsEarly, tsEarlyS5,
		[]map[string]any{
			testjsonl.GeminiUserMsg(
				"m1", tsEarly, "Hello Gemini",
			),
			testjsonl.GeminiAssistantMsg(
				"m2", tsEarlyS5, "Hi there!", nil,
			),
		},
	)

	path := env.writeGeminiSession(
		t,
		filepath.Join(
			"tmp", hash, "chats",
			"session-001.json",
		),
		content,
	)

	env.engine.SyncPaths([]string{path})

	assertSessionState(
		t, env.db, "gemini:"+sessionID,
		func(sess *db.Session) {
			if sess.Agent != "gemini" {
				t.Errorf("agent = %q, want gemini",
					sess.Agent)
			}
		},
	)
	assertSessionMessageCount(t, env.db, "gemini:"+sessionID, 2)
}

func TestSyncPathsCodexRejectsFlat(t *testing.T) {
	env := setupTestEnv(t)

	uuid := "d4e5f6a7-4567-8901-bcde-f12345678901"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			tsEarly, uuid,
			"/home/user/code/api", "user",
		).
		AddCodexMessage(tsEarlyS1, "user", "Add tests").
		String()

	// Write directly under codexDir (no year/month/day)
	path := env.writeSession(
		t, env.codexDir,
		"rollout-flat-"+uuid+".jsonl", content,
	)

	env.engine.SyncPaths([]string{path})

	sess, _ := env.db.GetSession(
		context.Background(), "codex:"+uuid,
	)
	if sess != nil {
		t.Error(
			"flat Codex file should be ignored " +
				"(no year/month/day structure)",
		)
	}
}

func TestSyncPathsGeminiRejectsWrongStructure(t *testing.T) {
	env := setupTestEnv(t)

	sessionID := "gem-wrong-struct"
	content := testjsonl.GeminiSessionJSON(
		sessionID, "somehash", tsEarly, tsEarlyS5,
		[]map[string]any{
			testjsonl.GeminiUserMsg(
				"m1", tsEarly, "Hello",
			),
		},
	)

	// Write session-*.json directly under geminiDir (wrong)
	path1 := env.writeGeminiSession(
		t, "session-wrong.json", content,
	)
	// Write under tmp/<hash> but without /chats/ dir
	path2 := env.writeGeminiSession(
		t,
		filepath.Join("tmp", "abc123", "session-bad.json"),
		content,
	)

	env.engine.SyncPaths([]string{path1, path2})

	sess, _ := env.db.GetSession(
		context.Background(), "gemini:"+sessionID,
	)
	if sess != nil {
		t.Error(
			"Gemini file outside tmp/<hash>/chats " +
				"should be ignored",
		)
	}
}

func TestSyncPathsAmp(t *testing.T) {
	env := setupTestEnv(t)

	content := `{"id":"T-019ca26f-aaaa-bbbb-cccc-dddddddddddd","created":1704103200000,"title":"Amp session","env":{"initial":{"trees":[{"displayName":"amp_proj"}]}},"messages":[{"role":"user","content":[{"type":"text","text":"hello from amp"}]},{"role":"assistant","content":[{"type":"text","text":"hi"}]}]}`

	path := env.writeAmpThread(
		t, "T-019ca26f-aaaa-bbbb-cccc-dddddddddddd.json",
		content,
	)

	env.engine.SyncPaths([]string{path})

	assertSessionState(
		t, env.db,
		"amp:T-019ca26f-aaaa-bbbb-cccc-dddddddddddd",
		func(sess *db.Session) {
			if sess.Agent != "amp" {
				t.Errorf("agent = %q, want amp", sess.Agent)
			}
		},
	)
	assertSessionMessageCount(
		t, env.db,
		"amp:T-019ca26f-aaaa-bbbb-cccc-dddddddddddd", 2,
	)

	updated := `{"id":"T-019ca26f-aaaa-bbbb-cccc-dddddddddddd","created":1704103200000,"title":"Amp session","env":{"initial":{"trees":[{"displayName":"amp_proj"}]}},"messages":[{"role":"user","content":[{"type":"text","text":"hello from amp"}]},{"role":"assistant","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"text","text":"incremental update"}]}]}`
	os.WriteFile(path, []byte(updated), 0o644)

	env.engine.SyncPaths([]string{path})

	assertSessionMessageCount(
		t, env.db,
		"amp:T-019ca26f-aaaa-bbbb-cccc-dddddddddddd", 3,
	)
}

func TestSyncPathsAmpRejectsWrongStructure(t *testing.T) {
	env := setupTestEnv(t)

	content := `{"id":"T-019ca26f-aaaa-bbbb-cccc-dddddddddddd","created":1704103200000,"title":"Amp session","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`

	// Nested paths under ampDir should be ignored.
	nested := env.writeAmpThread(
		t, filepath.Join("nested", "T-019ca26f-aaaa-bbbb-cccc-dddddddddddd.json"),
		content,
	)
	// Non-thread filename pattern at ampDir root should be ignored.
	wrongName := env.writeAmpThread(
		t, "thread-019ca26f-aaaa-bbbb-cccc-dddddddddddd.json",
		content,
	)
	// Malformed thread ID should be ignored.
	malformed := env.writeAmpThread(
		t, "T-.json",
		content,
	)

	env.engine.SyncPaths([]string{nested, wrongName, malformed})

	sess, _ := env.db.GetSession(
		context.Background(), "amp:T-019ca26f-aaaa-bbbb-cccc-dddddddddddd",
	)
	if sess != nil {
		t.Error("Amp files outside root-level valid T-<id>.json should be ignored")
	}
}

func TestSyncPathsStatsUpdated(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "Hello").
		String()

	path := env.writeClaudeSession(
		t, "proj", "stats-test.jsonl", content,
	)

	env.engine.SyncPaths([]string{path})

	stats := env.engine.LastSyncStats()
	if stats.Synced != 1 {
		t.Errorf("LastSyncStats.Synced = %d, want 1",
			stats.Synced)
	}
	lastSync := env.engine.LastSync()
	if lastSync.IsZero() {
		t.Error("LastSync should be set after SyncPaths")
	}
}

func TestSyncPathsClaudeParentSessionID(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID(
			tsZero, "Hello", "parent-uuid",
		).
		AddClaudeAssistant(tsZeroS5, "Hi there!").
		String()

	path := env.writeClaudeSession(
		t, "test-proj", "child-test.jsonl", content,
	)

	env.engine.SyncPaths([]string{path})

	assertSessionState(
		t, env.db, "child-test",
		func(sess *db.Session) {
			if sess.ParentSessionID == nil ||
				*sess.ParentSessionID != "parent-uuid" {
				t.Errorf("parent_session_id = %v, want %q",
					sess.ParentSessionID, "parent-uuid")
			}
		},
	)
}

func TestSyncPathsClaudeNoParentSessionID(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "Hello").
		String()

	path := env.writeClaudeSession(
		t, "test-proj", "no-parent-test.jsonl", content,
	)

	env.engine.SyncPaths([]string{path})

	assertSessionState(
		t, env.db, "no-parent-test",
		func(sess *db.Session) {
			if sess.ParentSessionID != nil {
				t.Errorf("parent_session_id = %v, want nil",
					sess.ParentSessionID)
			}
		},
	)
}

func TestSyncSubagentSetsParentSessionID(t *testing.T) {
	env := setupTestEnv(t)

	// Create parent session
	parentContent := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Build the feature").
		AddClaudeAssistant(tsEarlyS5, "On it.").
		String()

	env.writeClaudeSession(
		t, "test-proj", "parent-uuid.jsonl", parentContent,
	)

	// Create subagent file with sessionId pointing to parent
	subContent := testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID(
			tsEarly, "Do subtask", "parent-uuid",
		).
		AddClaudeAssistant(tsEarlyS5, "Subtask done.").
		String()

	env.writeSession(
		t, env.claudeDir,
		filepath.Join(
			"test-proj", "parent-uuid",
			"subagents", "agent-worker1.jsonl",
		),
		subContent,
	)

	// SyncAll should discover both parent and subagent
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 2, Synced: 2, Skipped: 0})

	// Verify parent has no parent_session_id
	assertSessionState(
		t, env.db, "parent-uuid",
		func(sess *db.Session) {
			if sess.ParentSessionID != nil {
				t.Errorf(
					"parent parent_session_id = %v, want nil",
					sess.ParentSessionID,
				)
			}
		},
	)

	// Verify subagent has parent_session_id set
	assertSessionState(
		t, env.db, "agent-worker1",
		func(sess *db.Session) {
			if sess.ParentSessionID == nil ||
				*sess.ParentSessionID != "parent-uuid" {
				t.Errorf(
					"subagent parent_session_id = %v, "+
						"want %q",
					sess.ParentSessionID, "parent-uuid",
				)
			}
			if sess.Agent != "claude" {
				t.Errorf("agent = %q, want claude",
					sess.Agent)
			}
		},
	)
	assertSessionMessageCount(t, env.db, "agent-worker1", 2)

	// Verify FindSourceFile works for subagent
	src := env.engine.FindSourceFile("agent-worker1")
	if src == "" {
		t.Error("FindSourceFile returned empty for subagent")
	}
}

func TestSyncPathsClaudeSubagent(t *testing.T) {
	env := setupTestEnv(t)

	// Create parent session
	parentContent := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "Hello").
		AddClaudeAssistant(tsZeroS5, "Hi!").
		String()

	env.writeClaudeSession(
		t, "test-proj", "parent-sess.jsonl", parentContent,
	)

	// Create subagent file with sessionId pointing to parent
	subagentContent := testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID(
			tsZero, "Do subtask", "parent-sess",
		).
		AddClaudeAssistant(tsZeroS5, "Done.").
		String()

	subPath := env.writeSession(
		t, env.claudeDir,
		filepath.Join(
			"test-proj", "parent-sess",
			"subagents", "agent-sub1.jsonl",
		),
		subagentContent,
	)

	// SyncPaths should accept the subagent path
	env.engine.SyncPaths([]string{subPath})

	assertSessionState(
		t, env.db, "agent-sub1",
		func(sess *db.Session) {
			if sess.Agent != "claude" {
				t.Errorf("agent = %q, want claude",
					sess.Agent)
			}
		},
	)
}

func TestSyncPathsClaudeRejectsNonAgentInSubagents(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "Hello").
		String()

	// Write a non-agent file in subagents dir
	path := env.writeSession(
		t, env.claudeDir,
		filepath.Join(
			"proj", "session",
			"subagents", "not-agent.jsonl",
		),
		content,
	)

	env.engine.SyncPaths([]string{path})

	sess, _ := env.db.GetSession(
		context.Background(), "not-agent",
	)
	if sess != nil {
		t.Error(
			"non-agent file in subagents dir " +
				"should be rejected",
		)
	}
}

func TestSyncPathsClaudeRejectsNested(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "Hello").
		String()

	// Write at proj/subdir/nested.jsonl — should be rejected
	// since Claude expects exactly <project>/<session>.jsonl.
	path := env.writeClaudeSession(
		t, filepath.Join("proj", "subdir"),
		"nested.jsonl", content,
	)

	env.engine.SyncPaths([]string{path})

	sess, _ := env.db.GetSession(
		context.Background(), "nested",
	)
	if sess != nil {
		t.Error(
			"nested Claude path should be rejected " +
				"(only <project>/<session>.jsonl allowed)",
		)
	}
}

// TestSyncEngineOpenCodeBulkSync verifies that SyncAll
// discovers OpenCode sessions and fully replaces messages
// when content changes in place (same ordinals, different
// text/tool data).
func TestSyncEngineOpenCodeBulkSync(t *testing.T) {
	env := setupTestEnv(t)

	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj-1", "/home/user/code/myapp")

	sessionID := "oc-sess-001"
	var timeCreated int64 = 1704067200000 // 2024-01-01T00:00:00Z
	var timeUpdated int64 = 1704067205000 // +5s

	oc.addSession(
		t, sessionID, "proj-1",
		timeCreated, timeUpdated,
	)
	oc.addMessage(
		t, "msg-u1", sessionID, "user", timeCreated,
	)
	oc.addMessage(
		t, "msg-a1", sessionID, "assistant",
		timeCreated+1,
	)
	oc.addTextPart(
		t, "part-u1", sessionID, "msg-u1",
		"original question", timeCreated,
	)
	oc.addTextPart(
		t, "part-a1", sessionID, "msg-a1",
		"original answer", timeCreated+1,
	)

	// First SyncAll should discover and store the session.
	env.engine.SyncAll(nil)

	agentviewID := "opencode:" + sessionID
	assertSessionState(t, env.db, agentviewID,
		func(sess *db.Session) {
			if sess.Agent != "opencode" {
				t.Errorf("agent = %q, want opencode",
					sess.Agent)
			}
		},
	)
	assertSessionMessageCount(t, env.db, agentviewID, 2)
	assertMessageContent(
		t, env.db, agentviewID,
		"original question", "original answer",
	)

	// Mutate the session in place: replace content but
	// keep the same number of messages (same ordinals).
	// Bump time_updated so the sync engine detects it.
	oc.replaceTextContent(
		t, sessionID,
		"updated question", "updated answer",
		timeCreated,
	)
	oc.updateSessionTime(t, sessionID, timeUpdated+1000)

	// Second SyncAll should fully replace messages.
	env.engine.SyncAll(nil)

	assertMessageContent(
		t, env.db, agentviewID,
		"updated question", "updated answer",
	)

	// Third SyncAll with no changes should be a no-op
	// (time_updated unchanged, so session is skipped).
	env.engine.SyncAll(nil)

	assertMessageContent(
		t, env.db, agentviewID,
		"updated question", "updated answer",
	)
}

// TestSyncEngineOpenCodeToolCallReplace verifies that tool
// call data is fully replaced during OpenCode bulk sync, not
// left stale from a previous sync.
func TestSyncEngineOpenCodeToolCallReplace(t *testing.T) {
	env := setupTestEnv(t)

	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj-1", "/home/user/code/myapp")

	sessionID := "oc-tool-sess"
	var timeCreated int64 = 1704067200000
	var timeUpdated int64 = 1704067205000

	oc.addSession(
		t, sessionID, "proj-1",
		timeCreated, timeUpdated,
	)

	// Assistant message with a tool call.
	oc.addMessage(
		t, "msg-u1", sessionID, "user", timeCreated,
	)
	oc.addMessage(
		t, "msg-a1", sessionID, "assistant",
		timeCreated+1,
	)
	oc.addTextPart(
		t, "part-u1", sessionID, "msg-u1",
		"run ls", timeCreated,
	)
	oc.addToolPart(
		t, "part-tool1", sessionID, "msg-a1",
		"bash", "call-1", timeCreated+1,
	)

	env.engine.SyncAll(nil)

	agentviewID := "opencode:" + sessionID
	assertToolCallCount(t, env.db, agentviewID, 1)

	// Replace: remove tool call, add text instead.
	oc.deleteMessages(t, sessionID)
	oc.deleteParts(t, sessionID)
	oc.addMessage(
		t, "msg-u1-v2", sessionID, "user", timeCreated,
	)
	oc.addMessage(
		t, "msg-a1-v2", sessionID, "assistant",
		timeCreated+1,
	)
	oc.addTextPart(
		t, "part-u1-v2", sessionID, "msg-u1-v2",
		"run ls", timeCreated,
	)
	oc.addTextPart(
		t, "part-a1-v2", sessionID, "msg-a1-v2",
		"here are the files", timeCreated+1,
	)
	oc.updateSessionTime(t, sessionID, timeUpdated+1000)

	env.engine.SyncAll(nil)

	assertMessageContent(
		t, env.db, agentviewID,
		"run ls", "here are the files",
	)
	assertToolCallCount(t, env.db, agentviewID, 0)
}

// TestSyncEngineConcurrentSerialization verifies that
// SyncAll and ResyncAll are serialized by syncMu.
//
// Strategy: SyncAll's progress callback blocks on a
// barrier channel, holding the mutex. A second goroutine
// launches ResyncAll and signals when it enters its own
// progress callback. If the mutex works, the second
// signal only arrives after the barrier is released.
func TestSyncEngineConcurrentSerialization(t *testing.T) {
	env := setupTestEnv(t)

	for i := range 3 {
		content := testjsonl.NewSessionBuilder().
			AddClaudeUser(tsZero, fmt.Sprintf("msg %d", i)).
			String()
		env.writeClaudeSession(
			t, "proj",
			fmt.Sprintf("conc-%d.jsonl", i), content,
		)
	}

	// barrier blocks SyncAll's progress callback,
	// keeping syncMu held.
	barrier := make(chan struct{})
	// syncAllEntered signals that SyncAll is inside
	// the mutex-protected section.
	syncAllEntered := make(chan struct{})
	// resyncEntered signals that ResyncAll reached its
	// progress callback (i.e. acquired the mutex).
	resyncEntered := make(chan struct{})

	var syncOnce, resyncOnce gosync.Once

	syncProgress := func(_ sync.Progress) {
		syncOnce.Do(func() {
			close(syncAllEntered)
			<-barrier // hold mutex until released
		})
	}

	resyncProgress := func(_ sync.Progress) {
		resyncOnce.Do(func() {
			close(resyncEntered)
		})
	}

	var wg gosync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		env.engine.SyncAll(syncProgress)
	}()

	// Wait until SyncAll is inside the locked section.
	<-syncAllEntered

	go func() {
		defer wg.Done()
		env.engine.ResyncAll(resyncProgress)
	}()

	// ResyncAll should be blocked on the mutex. Give it
	// a moment to prove it can't enter.
	select {
	case <-resyncEntered:
		t.Fatal(
			"ResyncAll entered while SyncAll held mutex",
		)
	case <-time.After(50 * time.Millisecond):
		// Expected: ResyncAll is blocked.
	}

	// Release the barrier so SyncAll finishes.
	close(barrier)

	// Now ResyncAll should proceed.
	select {
	case <-resyncEntered:
		// Expected: ResyncAll acquired mutex.
	case <-time.After(5 * time.Second):
		t.Fatal("ResyncAll never entered after barrier release")
	}

	wg.Wait()
}

// TestSyncEnginePostFilterCounts verifies that writeBatch
// stores post-filter message counts (after pairAndFilter
// removes empty user+tool_result messages), not the raw
// parser counts.
func TestSyncEnginePostFilterCounts(t *testing.T) {
	env := setupTestEnv(t)

	// Build a session with 4 raw messages:
	//   1. user with content (kept)
	//   2. assistant with tool_use (kept)
	//   3. user with only tool_result, no text (filtered)
	//   4. assistant with text (kept)
	// Post-filter: 3 messages, 1 user message.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Read main.go").
		AddRaw(testjsonl.ClaudeAssistantJSON(
			[]map[string]any{{
				"type": "tool_use",
				"id":   "toolu_1",
				"name": "Read",
				"input": map[string]string{
					"file_path": "main.go",
				},
			}},
			tsEarlyS1,
		)).
		AddRaw(testjsonl.ClaudeToolResultUserJSON(
			"toolu_1", "package main", tsEarlyS5,
		)).
		AddClaudeAssistant(tsEarlyS5, "Here it is.").
		String()

	env.writeClaudeSession(
		t, "test-proj",
		"filter-count.jsonl", content,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1 + 0, Synced: 1, Skipped: 0})

	// Verify stored counts match post-filter values.
	assertSessionMessageCount(t, env.db, "filter-count", 3)
	assertSessionState(t, env.db, "filter-count", func(sess *db.Session) {
		if sess.UserMessageCount != 1 {
			t.Errorf("user_message_count = %d, want 1", sess.UserMessageCount)
		}
	})
}

// TestSyncSingleSessionPostFilterCounts verifies that
// writeSessionFull (used by SyncSingleSession) also stores
// post-filter counts.
func TestSyncSingleSessionPostFilterCounts(t *testing.T) {
	env := setupTestEnv(t)

	content2 := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Read main.go").
		AddRaw(testjsonl.ClaudeAssistantJSON(
			[]map[string]any{{
				"type": "tool_use",
				"id":   "toolu_1",
				"name": "Read",
				"input": map[string]string{
					"file_path": "main.go",
				},
			}},
			tsEarlyS1,
		)).
		AddRaw(testjsonl.ClaudeToolResultUserJSON(
			"toolu_1", "package main", tsEarlyS5,
		)).
		AddClaudeAssistant(tsEarlyS5, "Here it is.").
		String()

	env.writeClaudeSession(
		t, "test-proj",
		"filter-single.jsonl", content2,
	)

	// SyncAll to populate the session in the DB.
	env.engine.SyncAll(nil)

	// Corrupt stored counts and clear mtime so
	// SyncSingleSession re-parses via writeSessionFull.
	err := env.db.Update(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			"UPDATE sessions"+
				" SET message_count = 999,"+
				" user_message_count = 999,"+
				" file_mtime = NULL"+
				" WHERE id = ?",
			"filter-single",
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return fmt.Errorf(
				"expected 1 row affected, got %d", n,
			)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("corrupt counts: %v", err)
	}

	if err := env.engine.SyncSingleSession(
		"filter-single",
	); err != nil {
		t.Fatalf("SyncSingleSession: %v", err)
	}

	// Counts should be corrected by writeSessionFull.
	assertSessionMessageCount(t, env.db, "filter-single", 3)
	assertSessionState(t, env.db, "filter-single", func(sess *db.Session) {
		if sess.UserMessageCount != 1 {
			t.Errorf("user_message_count = %d, want 1", sess.UserMessageCount)
		}
	})
}

func TestSyncEngineMultiClaudeDir(t *testing.T) {
	claudeDir1 := t.TempDir()
	claudeDir2 := t.TempDir()
	env := setupTestEnv(t, WithClaudeDirs([]string{claudeDir1, claudeDir2}))

	content1 := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Hello from dir1").
		String()
	content2 := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Hello from dir2").
		String()

	// Write sessions to different directories
	path1 := filepath.Join(claudeDir1, "proj1", "sess1.jsonl")
	dbtest.WriteTestFile(t, path1, []byte(content1))

	path2 := filepath.Join(claudeDir2, "proj2", "sess2.jsonl")
	dbtest.WriteTestFile(t, path2, []byte(content2))

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 2, Synced: 2, Skipped: 0})

	assertSessionMessageCount(t, env.db, "sess1", 1)
	assertSessionMessageCount(t, env.db, "sess2", 1)

	// SyncPaths should work across directories
	appended := content1 + testjsonl.NewSessionBuilder().
		AddClaudeAssistant(tsEarlyS5, "Reply").
		String()
	os.WriteFile(path1, []byte(appended), 0o644)
	env.engine.SyncPaths([]string{path1})

	assertSessionMessageCount(t, env.db, "sess1", 2)

	// FindSourceFile should search across directories
	src := env.engine.FindSourceFile("sess2")
	if src == "" {
		t.Error("FindSourceFile failed for sess2 in second directory")
	}
}

// TestSyncEngineMultiCursorDir verifies that SyncAll and
// SyncPaths work when multiple Cursor project directories
// are configured, and that the containment check in
// processCursor correctly identifies the containing root
// for files under non-first directories.
func TestSyncEngineMultiCursorDir(t *testing.T) {
	cursorDir1 := t.TempDir()
	cursorDir2 := t.TempDir()
	env := setupTestEnv(
		t, WithCursorDirs([]string{cursorDir1, cursorDir2}),
	)

	transcript1 := "user:\nHello from cursor dir1\n" +
		"assistant:\nHi from dir1!\n"
	transcript2 := "user:\nHello from cursor dir2\n" +
		"assistant:\nHi from dir2!\n"

	// Write sessions to different Cursor directories.
	// Cursor project dir uses hyphenated encoding.
	env.writeCursorSession(
		t, cursorDir1,
		"Users-alice-code-proj1",
		"sess1.txt", transcript1,
	)
	path2 := env.writeCursorSession(
		t, cursorDir2,
		"Users-alice-code-proj2",
		"sess2.txt", transcript2,
	)

	// SyncAll should discover sessions from both dirs.
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2, Synced: 2, Skipped: 0,
	})

	assertSessionState(
		t, env.db, "cursor:sess1",
		func(sess *db.Session) {
			if sess.Agent != "cursor" {
				t.Errorf(
					"agent = %q, want cursor",
					sess.Agent,
				)
			}
		},
	)
	assertSessionState(
		t, env.db, "cursor:sess2",
		func(sess *db.Session) {
			if sess.Agent != "cursor" {
				t.Errorf(
					"agent = %q, want cursor",
					sess.Agent,
				)
			}
		},
	)

	// SyncPaths should handle a file from the second dir.
	updated := "user:\nHello from cursor dir2\n" +
		"assistant:\nHi from dir2!\n" +
		"user:\nFollow-up\n" +
		"assistant:\nGot it.\n"
	os.WriteFile(path2, []byte(updated), 0o644)
	env.engine.SyncPaths([]string{path2})

	assertSessionMessageCount(t, env.db, "cursor:sess2", 4)

	// FindSourceFile should work across directories.
	src := env.engine.FindSourceFile("cursor:sess2")
	if src == "" {
		t.Error(
			"FindSourceFile failed for cursor:sess2 " +
				"in second directory",
		)
	}
}

func TestSyncForkDetection(t *testing.T) {
	env := setupTestEnv(t)

	// Main branch: a->b->c->d->e->f->g->h->k->l (5 user turns)
	// Fork from b: i->j (1 user turn on fork branch)
	// First branch from b has 4 user turns (c,e,g,k) > 3 = large gap
	content := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID("2024-01-01T10:00:00Z", "start", "a", "").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:01Z", "ok", "b", "a").
		AddClaudeUserWithUUID("2024-01-01T10:00:02Z", "step2", "c", "b").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:03Z", "ok2", "d", "c").
		AddClaudeUserWithUUID("2024-01-01T10:00:04Z", "step3", "e", "d").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:05Z", "ok3", "f", "e").
		AddClaudeUserWithUUID("2024-01-01T10:00:06Z", "step4", "g", "f").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:07Z", "ok4", "h", "g").
		AddClaudeUserWithUUID("2024-01-01T10:00:08Z", "step5", "k", "h").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:09Z", "ok5", "l", "k").
		AddClaudeUserWithUUID("2024-01-01T10:01:00Z", "fork-start", "i", "b").
		AddClaudeAssistantWithUUID("2024-01-01T10:01:01Z", "fork-ok", "j", "i").
		String()

	env.writeClaudeSession(t, "test-proj", "parent-uuid.jsonl", content)
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1, Synced: 2, Skipped: 0})

	assertSessionMessageCount(t, env.db, "parent-uuid", 10)
	assertSessionMessageCount(t, env.db, "parent-uuid-i", 2)

	assertSessionState(t, env.db, "parent-uuid-i", func(sess *db.Session) {
		if sess.ParentSessionID == nil || *sess.ParentSessionID != "parent-uuid" {
			t.Errorf("fork parent = %v, want parent-uuid", sess.ParentSessionID)
		}
		if sess.RelationshipType != "fork" {
			t.Errorf("fork relationship_type = %q, want fork", sess.RelationshipType)
		}
	})
}

func TestSyncSmallGapRetry(t *testing.T) {
	env := setupTestEnv(t)

	// Main: a->b->c->d (1 user turn after fork point = small gap)
	// Retry from b: e->f
	content := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID("2024-01-01T10:00:00Z", "start", "a", "").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:01Z", "ok", "b", "a").
		AddClaudeUserWithUUID("2024-01-01T10:00:02Z", "try1", "c", "b").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:03Z", "resp1", "d", "c").
		AddClaudeUserWithUUID("2024-01-01T10:01:00Z", "try2", "e", "b").
		AddClaudeAssistantWithUUID("2024-01-01T10:01:01Z", "resp2", "f", "e").
		String()

	env.writeClaudeSession(t, "test-proj", "retry-uuid.jsonl", content)
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1, Synced: 1, Skipped: 0})

	assertSessionMessageCount(t, env.db, "retry-uuid", 4)
}

func TestResyncAllReplacesMessageContent(t *testing.T) {
	env := setupTestEnv(t)

	sessionID := "gem-resync-test"
	hash := "resync123"

	content := testjsonl.GeminiSessionJSON(
		sessionID, hash, tsEarly, tsEarlyS5,
		[]map[string]any{
			testjsonl.GeminiUserMsg(
				"u1", tsEarly, "Explain this code",
			),
			testjsonl.GeminiAssistantMsg(
				"a1", tsEarlyS5, "Here is the explanation.",
				&testjsonl.GeminiMsgOpts{
					Thoughts: []testjsonl.GeminiThought{{
						Subject:     "Analysis",
						Description: "Reading the code",
						Timestamp:   tsEarlyS1,
					}},
				},
			),
		},
	)

	relPath := filepath.Join(
		"tmp", hash, "chats", "session-001.json",
	)
	env.writeGeminiSession(t, relPath, content)

	// Initial sync.
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	fullID := "gemini:" + sessionID
	msgs := fetchMessages(t, env.db, fullID)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}

	// Simulate a parser change by directly modifying message
	// content in the DB. This mirrors what happens when the Go
	// parser is updated (e.g. thinking format change) but the
	// source files on disk are unchanged.
	err := env.db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"UPDATE messages SET content = ?"+
				" WHERE session_id = ? AND ordinal = 1",
			"stale content from old parser",
			fullID,
		)
		return err
	})
	if err != nil {
		t.Fatalf("update message content: %v", err)
	}

	// Normal SyncAll should skip (file unchanged on disk).
	stats := env.engine.SyncAll(nil)
	if stats.Skipped != 1 {
		t.Fatalf("expected 1 skip, got %d", stats.Skipped)
	}
	msgs = fetchMessages(t, env.db, fullID)
	if !strings.Contains(msgs[1].Content, "stale content") {
		t.Fatal("SyncAll should not have replaced content")
	}

	// Capture FTS state before resync so a regression that
	// breaks FTS isn't masked by HasFTS() returning false
	// post-resync.
	hadFTS := env.db.HasFTS()

	// ResyncAll should re-parse and replace message content.
	env.engine.ResyncAll(nil)
	msgs = fetchMessages(t, env.db, fullID)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages after resync, want 2",
			len(msgs))
	}
	if strings.Contains(msgs[1].Content, "stale content") {
		t.Error(
			"ResyncAll did not replace message content",
		)
	}
	if !strings.Contains(
		msgs[1].Content, "Here is the explanation.",
	) {
		t.Errorf(
			"unexpected content after resync: %q",
			msgs[1].Content,
		)
	}

	// FTS search should work after resync (index was dropped
	// and rebuilt).
	if hadFTS {
		if !env.db.HasFTS() {
			t.Fatal(
				"FTS available before resync but not after",
			)
		}
		page, err := env.db.Search(
			context.Background(),
			db.SearchFilter{Query: "explanation"},
		)
		if err != nil {
			t.Fatalf("search after resync: %v", err)
		}
		if len(page.Results) == 0 {
			t.Error(
				"FTS search returned no results after resync",
			)
		}
	}
}

func TestResyncAllPreservesInsights(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Hello").
		AddClaudeAssistant(tsEarlyS5, "Hi there!").
		String()

	env.writeClaudeSession(
		t, "test-proj", "insight-test.jsonl", content,
	)

	env.engine.SyncAll(nil)
	assertSessionMessageCount(t, env.db, "insight-test", 2)

	// Insert an insight into the DB.
	_, err := env.db.InsertInsight(db.Insight{
		Type:     "daily_activity",
		DateFrom: "2025-01-15",
		DateTo:   "2025-01-15",
		Agent:    "claude",
		Content:  "test insight survives resync",
	})
	if err != nil {
		t.Fatalf("InsertInsight: %v", err)
	}

	// ResyncAll should rebuild sessions and preserve
	// insights.
	stats := env.engine.ResyncAll(nil)
	if stats.Synced == 0 {
		t.Fatal("expected at least 1 synced session")
	}

	assertSessionMessageCount(t, env.db, "insight-test", 2)

	insights, err := env.db.ListInsights(
		context.Background(), db.InsightFilter{},
	)
	if err != nil {
		t.Fatalf("ListInsights: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("got %d insights, want 1", len(insights))
	}
	if insights[0].Content != "test insight survives resync" {
		t.Errorf(
			"insight content = %q, want preserved",
			insights[0].Content,
		)
	}
}

// TestResyncAllAbortsOnFailures verifies that ResyncAll
// does not swap the DB when sync has more failures than
// successes.
func TestResyncAllAbortsOnFailures(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "original content").
		AddClaudeAssistant(tsEarlyS5, "original reply").
		String()

	env.writeClaudeSession(
		t, "test-proj", "abort-test.jsonl", content,
	)

	env.engine.SyncAll(nil)
	assertSessionMessageCount(t, env.db, "abort-test", 2)

	if runtime.GOOS == "windows" {
		t.Skip("chmod(0) does not prevent reads on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root can read mode-0 files")
	}

	// Make the file unreadable so the parser returns a hard
	// error. This is deterministic: os.Open will fail with
	// a permission error on every attempt.
	sessionPath := filepath.Join(
		env.claudeDir, "test-proj", "abort-test.jsonl",
	)
	if err := os.Chmod(sessionPath, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		os.Chmod(sessionPath, 0o644)
	})

	stats := env.engine.ResyncAll(nil)

	if stats.Failed == 0 {
		t.Fatalf("expected failures, got 0")
	}
	if stats.TotalSessions == 0 {
		t.Fatal("expected TotalSessions > 0")
	}

	if !stats.Aborted {
		t.Error("expected Aborted = true")
	}

	hasAbortWarning := false
	for _, w := range stats.Warnings {
		if strings.Contains(w, "resync aborted") {
			hasAbortWarning = true
		}
	}
	if !hasAbortWarning {
		t.Error("expected 'resync aborted' warning")
	}

	// Original data should be preserved since swap was
	// aborted.
	assertSessionMessageCount(t, env.db, "abort-test", 2)
	assertMessageContent(
		t, env.db, "abort-test",
		"original content", "original reply",
	)
}

// TestResyncAllAbortsWithForkAndFailures exercises the abort
// guard's file-level counting. A fork-producing file yields
// Synced=2 from filesOK=1. Two unreadable files add Failed=2.
// The abort guard should fire because Failed(2) > filesOK(1),
// even though Failed(2) == Synced(2) would pass a naive
// session-level comparison.
func TestResyncAllAbortsWithForkAndFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod(0) does not prevent reads on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root can read mode-0 files")
	}

	env := setupTestEnv(t)

	// File 1: fork-producing session (1 file → 2 sessions).
	// Main branch has 5 user turns; fork from b creates a
	// 4-turn gap (>3) which triggers fork detection.
	forkContent := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:00Z", "start", "a", "",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:01Z", "ok", "b", "a",
		).
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:02Z", "s2", "c", "b",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:03Z", "ok2", "d", "c",
		).
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:04Z", "s3", "e", "d",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:05Z", "ok3", "f", "e",
		).
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:06Z", "s4", "g", "f",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:07Z", "ok4", "h", "g",
		).
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:08Z", "s5", "k", "h",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:09Z", "ok5", "l", "k",
		).
		AddClaudeUserWithUUID(
			"2024-01-01T10:01:00Z", "fork", "i", "b",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:01:01Z", "fork-ok", "j", "i",
		).
		String()

	env.writeClaudeSession(
		t, "proj", "forked.jsonl", forkContent,
	)

	// Files 2 & 3: normal sessions that we'll make unreadable.
	for _, name := range []string{"bad1.jsonl", "bad2.jsonl"} {
		c := testjsonl.NewSessionBuilder().
			AddClaudeUser(tsEarly, "hello").
			String()
		env.writeClaudeSession(t, "proj", name, c)
	}

	// Initial sync: all 3 files parse fine.
	// Fork file produces 2 sessions: "forked" (10 msgs)
	// and "forked-i" (2 msgs).
	env.engine.SyncAll(nil)
	assertSessionMessageCount(t, env.db, "forked", 10)
	assertSessionMessageCount(t, env.db, "forked-i", 2)

	// Make both normal files unreadable.
	for _, name := range []string{"bad1.jsonl", "bad2.jsonl"} {
		p := filepath.Join(env.claudeDir, "proj", name)
		if err := os.Chmod(p, 0); err != nil {
			t.Fatalf("chmod %s: %v", name, err)
		}
		t.Cleanup(func() { os.Chmod(p, 0o644) })
	}

	stats := env.engine.ResyncAll(nil)

	// Expect: filesOK=1, Failed=2, Synced=2.
	// Abort should fire because Failed(2) > filesOK(1).
	if stats.Failed != 2 {
		t.Fatalf("Failed = %d, want 2", stats.Failed)
	}
	if stats.Synced != 2 {
		t.Fatalf("Synced = %d, want 2", stats.Synced)
	}

	hasAbortWarning := false
	for _, w := range stats.Warnings {
		if strings.Contains(w, "resync aborted") {
			hasAbortWarning = true
		}
	}
	if !hasAbortWarning {
		t.Error(
			"expected abort: Failed(2) > filesOK(1) " +
				"should trigger even though Failed == Synced",
		)
	}

	// Original data preserved.
	assertSessionMessageCount(t, env.db, "forked", 10)
	assertSessionMessageCount(t, env.db, "forked-i", 2)
}

// TestResyncAllPostReopenAvailability verifies that reads and
// writes work through the DB handle after ResyncAll completes
// the close-rename-reopen cycle.
func TestResyncAllPostReopenAvailability(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "availability check").
		AddClaudeAssistant(tsEarlyS5, "still here").
		String()

	env.writeClaudeSession(
		t, "avail-proj", "avail.jsonl", content,
	)

	// Initial sync.
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	// Resync triggers the full close-rename-reopen cycle.
	stats := env.engine.ResyncAll(nil)
	if stats.Synced != 1 {
		t.Fatalf("resync: synced = %d, want 1", stats.Synced)
	}
	if stats.Aborted {
		t.Error("unexpected Aborted = true on successful resync")
	}
	for _, w := range stats.Warnings {
		t.Errorf("unexpected warning: %s", w)
	}

	// Verify reads work on the reopened DB.
	s, err := env.db.GetSession(
		context.Background(), "avail",
	)
	if err != nil {
		t.Fatalf("GetSession after resync: %v", err)
	}
	if s == nil {
		t.Fatal("session missing after resync")
	}

	msgs := fetchMessages(t, env.db, "avail")
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}

	// Verify writes work on the reopened DB.
	err = env.db.UpsertSession(db.Session{
		ID:           "post-resync-write",
		Project:      "avail-proj",
		Machine:      "local",
		Agent:        "claude",
		MessageCount: 1,
	})
	if err != nil {
		t.Fatalf("UpsertSession after resync: %v", err)
	}
	s2, err := env.db.GetSession(
		context.Background(), "post-resync-write",
	)
	if err != nil {
		t.Fatalf("GetSession post-write: %v", err)
	}
	if s2 == nil {
		t.Fatal("session written after resync not found")
	}

	// Verify a subsequent SyncAll still works (engine state
	// is consistent with the reopened DB).
	stats2 := env.engine.SyncAll(nil)
	if stats2.Synced != 0 || stats2.Skipped != 1 {
		t.Errorf(
			"post-resync SyncAll: synced=%d skipped=%d",
			stats2.Synced, stats2.Skipped,
		)
	}
}

// TestResyncAllConcurrentReads verifies that concurrent reads
// through the DB handle don't panic or deadlock while ResyncAll
// runs the close-rename-reopen cycle.
func TestResyncAllConcurrentReads(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "concurrent engine").
		AddClaudeAssistant(tsEarlyS5, "response").
		String()

	env.writeClaudeSession(
		t, "conc-proj", "conc.jsonl", content,
	)

	env.engine.SyncAll(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg gosync.WaitGroup

	// Start readers before resync.
	readersReady := make(chan struct{})
	var readyCount gosync.WaitGroup
	readyCount.Add(4)

	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				s, _ := env.db.GetSession(ctx, "conc")
				// Signal ready after first successful read.
				if s != nil {
					readyCount.Done()
					break
				}
			}
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				// Ignore errors; the test verifies no
				// panics/deadlocks and post-resync health.
				env.db.GetSession(ctx, "conc")
			}
		})
	}

	// Wait for all readers to complete one successful read.
	go func() {
		readyCount.Wait()
		close(readersReady)
	}()
	<-readersReady

	// Run resync while readers are active.
	stats := env.engine.ResyncAll(nil)
	cancel()
	wg.Wait()

	if stats.Synced != 1 {
		t.Fatalf("resync: synced = %d, want 1", stats.Synced)
	}

	// Post-resync reads must succeed.
	s, err := env.db.GetSession(
		context.Background(), "conc",
	)
	if err != nil {
		t.Fatalf("GetSession after resync: %v", err)
	}
	if s == nil {
		t.Fatal("session missing after resync")
	}
}

// TestResyncAllAbortsOnEmptyDiscovery verifies that resync does
// not replace a populated DB with an empty one when discovery
// returns zero files (e.g. session directories are temporarily
// inaccessible or misconfigured).
func TestResyncAllAbortsOnEmptyDiscovery(t *testing.T) {
	env := setupTestEnv(t)

	// Seed existing data via initial sync.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "keep me").
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()
	env.writeClaudeSession(t, "proj", "keep.jsonl", content)
	env.engine.SyncAll(nil)
	assertSessionMessageCount(t, env.db, "keep", 2)

	// Remove all session files to simulate empty discovery.
	entries, err := os.ReadDir(
		filepath.Join(env.claudeDir, "proj"),
	)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	for _, e := range entries {
		p := filepath.Join(env.claudeDir, "proj", e.Name())
		os.Remove(p)
	}

	stats := env.engine.ResyncAll(nil)

	// Swap must be aborted.
	hasAbortWarning := false
	for _, w := range stats.Warnings {
		if strings.Contains(w, "resync aborted") {
			hasAbortWarning = true
		}
	}
	if !hasAbortWarning {
		t.Error(
			"expected abort when discovery returns zero files " +
				"but old DB has sessions",
		)
	}

	// Original data must be preserved.
	assertSessionMessageCount(t, env.db, "keep", 2)
}

// TestResyncAllOpenCodeOnly verifies that ResyncAll succeeds
// when only OpenCode sessions exist (no file-based sessions).
// The empty-discovery guard must not abort when OpenCode
// sessions are synced.
func TestResyncAllOpenCodeOnly(t *testing.T) {
	env := setupTestEnv(t)

	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj-1", "/home/user/code/myapp")

	sessionID := "oc-resync-only"
	var timeCreated int64 = 1704067200000
	var timeUpdated int64 = 1704067205000

	oc.addSession(
		t, sessionID, "proj-1",
		timeCreated, timeUpdated,
	)
	oc.addMessage(
		t, "msg-u1", sessionID, "user", timeCreated,
	)
	oc.addMessage(
		t, "msg-a1", sessionID, "assistant",
		timeCreated+1,
	)
	oc.addTextPart(
		t, "part-u1", sessionID, "msg-u1",
		"hello opencode", timeCreated,
	)
	oc.addTextPart(
		t, "part-a1", sessionID, "msg-a1",
		"hi there", timeCreated+1,
	)

	// Initial sync populates the DB with OpenCode sessions.
	env.engine.SyncAll(nil)
	agentviewID := "opencode:" + sessionID
	assertSessionMessageCount(t, env.db, agentviewID, 2)

	// ResyncAll must not abort — OpenCode sessions should
	// survive even though file discovery returns zero.
	stats := env.engine.ResyncAll(nil)

	for _, w := range stats.Warnings {
		if strings.Contains(w, "resync aborted") {
			t.Fatalf(
				"ResyncAll aborted for OpenCode-only "+
					"dataset: %s", w,
			)
		}
	}
	if stats.Synced == 0 {
		t.Fatal("expected OpenCode sessions to be synced")
	}

	assertSessionMessageCount(t, env.db, agentviewID, 2)
	assertMessageContent(
		t, env.db, agentviewID,
		"hello opencode", "hi there",
	)
}

// TestResyncAllAbortsMixedSourceEmptyFiles verifies that
// ResyncAll aborts when the old DB has both file-backed and
// OpenCode sessions but file discovery returns zero (e.g.
// file dirs temporarily inaccessible). OpenCode sync
// succeeding must not mask the loss of file-backed sessions.
func TestResyncAllAbortsMixedSourceEmptyFiles(t *testing.T) {
	env := setupTestEnv(t)

	// Seed file-backed sessions.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "file session").
		AddClaudeAssistant(tsEarlyS5, "file reply").
		String()
	env.writeClaudeSession(
		t, "proj", "mixed-file.jsonl", content,
	)

	// Seed OpenCode sessions.
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj-1", "/home/user/code/myapp")

	sessionID := "oc-mixed"
	var timeCreated int64 = 1704067200000
	var timeUpdated int64 = 1704067205000

	oc.addSession(
		t, sessionID, "proj-1",
		timeCreated, timeUpdated,
	)
	oc.addMessage(
		t, "msg-u1", sessionID, "user", timeCreated,
	)
	oc.addMessage(
		t, "msg-a1", sessionID, "assistant",
		timeCreated+1,
	)
	oc.addTextPart(
		t, "part-u1", sessionID, "msg-u1",
		"oc question", timeCreated,
	)
	oc.addTextPart(
		t, "part-a1", sessionID, "msg-a1",
		"oc answer", timeCreated+1,
	)

	// Initial sync: both sources.
	env.engine.SyncAll(nil)
	assertSessionMessageCount(t, env.db, "mixed-file", 2)
	assertSessionMessageCount(
		t, env.db, "opencode:"+sessionID, 2,
	)

	// Remove all file-based sessions to simulate empty
	// file discovery. OpenCode data remains.
	entries, err := os.ReadDir(
		filepath.Join(env.claudeDir, "proj"),
	)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	for _, e := range entries {
		p := filepath.Join(env.claudeDir, "proj", e.Name())
		os.Remove(p)
	}

	stats := env.engine.ResyncAll(nil)

	// Must abort: file-backed sessions would be lost.
	hasAbortWarning := false
	for _, w := range stats.Warnings {
		if strings.Contains(w, "resync aborted") {
			hasAbortWarning = true
		}
	}
	if !hasAbortWarning {
		t.Error(
			"expected abort when file dirs are empty " +
				"but old DB has file-backed sessions",
		)
	}

	// Both file-backed and OpenCode data preserved.
	assertSessionMessageCount(t, env.db, "mixed-file", 2)
	assertSessionMessageCount(
		t, env.db, "opencode:"+sessionID, 2,
	)
}

// TestNewEngineDefensiveCopy verifies that NewEngine deep-copies
// the AgentDirs map so that external mutations after construction
// do not affect the engine's behavior.
func TestNewEngineDefensiveCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	claudeDir := t.TempDir()
	database := dbtest.OpenTestDB(t)

	dirs := map[parser.AgentType][]string{
		parser.AgentClaude: {claudeDir},
	}
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: dirs,
		Machine:   "local",
	})

	// Write a session the engine should find.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "hello").
		String()
	path := filepath.Join(
		claudeDir, "proj", "copy-test.jsonl",
	)
	dbtest.WriteTestFile(t, path, []byte(content))

	// Mutate the original map after construction: clear
	// the Claude dirs and add a bogus entry.
	dirs[parser.AgentClaude] = nil
	dirs[parser.AgentCodex] = []string{"/bogus"}

	// Engine should still find the session via its own copy.
	stats := engine.SyncAll(nil)
	if stats.Synced != 1 {
		t.Fatalf(
			"Synced = %d, want 1 (engine used mutated map)",
			stats.Synced,
		)
	}
	assertSessionMessageCount(t, database, "copy-test", 1)

	// Verify slice-level aliasing is also prevented.
	// Build a fresh engine where we mutate an element
	// inside the original slice (not replace the slice).
	claudeDir2 := t.TempDir()
	sliceDirs := []string{claudeDir2}
	dirs2 := map[parser.AgentType][]string{
		parser.AgentClaude: sliceDirs,
	}
	db2 := dbtest.OpenTestDB(t)
	engine2 := sync.NewEngine(db2, sync.EngineConfig{
		AgentDirs: dirs2,
		Machine:   "local",
	})

	content2 := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "slice test").
		String()
	path2 := filepath.Join(
		claudeDir2, "proj", "slice-test.jsonl",
	)
	dbtest.WriteTestFile(t, path2, []byte(content2))

	// Mutate the element inside the original slice.
	sliceDirs[0] = "/nonexistent"

	stats2 := engine2.SyncAll(nil)
	if stats2.Synced != 1 {
		t.Fatalf(
			"Synced = %d, want 1 (engine used aliased slice)",
			stats2.Synced,
		)
	}
	assertSessionMessageCount(t, db2, "slice-test", 1)
}

// TestSyncPathsClaudeFallsThrough verifies that a file under
// a Claude root that fails the subagent shape check (non-agent-
// prefix in a subagents dir) is still checked against later
// agents when roots overlap.
func TestSyncPathsClaudeFallsThrough(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Claude root contains a path that looks like a subagent
	// dir but the file doesn't have an agent- prefix. The Amp
	// root is nested so the same file matches Amp's structure.
	parent := t.TempDir()
	claudeDir := parent
	// Amp root: <claudeDir>/proj/sess/subagents
	ampDir := filepath.Join(
		claudeDir, "proj", "sess", "subagents",
	)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir},
			parser.AgentAmp:    {ampDir},
		},
		Machine: "local",
	})

	content := `{"id":"T-019ca26f-eeee-dddd-cccc-bbbbbbbbbbbb","created":1704103200000,"title":"Claude overlap","env":{"initial":{"trees":[{"displayName":"proj"}]}},"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]},{"role":"assistant","content":[{"type":"text","text":"hi"}]}]}`

	// This path is 4 parts under claudeDir
	// (proj/sess/subagents/T-*.json) and matches the
	// subagent shape check, but the filename doesn't start
	// with "agent-", so Claude rejects it. It should fall
	// through to Amp.
	ampPath := filepath.Join(
		ampDir,
		"T-019ca26f-eeee-dddd-cccc-bbbbbbbbbbbb.json",
	)
	dbtest.WriteTestFile(t, ampPath, []byte(content))

	engine.SyncPaths([]string{ampPath})

	assertSessionState(
		t, database,
		"amp:T-019ca26f-eeee-dddd-cccc-bbbbbbbbbbbb",
		func(sess *db.Session) {
			if sess.Agent != "amp" {
				t.Errorf(
					"agent = %q, want amp", sess.Agent,
				)
			}
		},
	)
}

// TestSyncPathsClassifyFallsThrough verifies that a file
// under a Cursor root that doesn't match the Cursor transcript
// structure is still checked against later agents (e.g. Amp).
// Before the fix, the Cursor block returned false immediately,
// preventing any subsequent agent from matching.
func TestSyncPathsClassifyFallsThrough(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Use a shared parent dir so both agent roots overlap:
	// cursorDir = parent/cursor
	// ampDir    = parent/cursor/nested-amp
	// A valid Amp file under ampDir also lives under
	// cursorDir but doesn't match the Cursor structure.
	parent := t.TempDir()
	cursorDir := filepath.Join(parent, "cursor")
	ampDir := filepath.Join(cursorDir, "nested-amp")

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCursor: {cursorDir},
			parser.AgentAmp:    {ampDir},
		},
		Machine: "local",
	})

	content := `{"id":"T-019ca26f-ffff-aaaa-bbbb-cccccccccccc","created":1704103200000,"title":"Overlap test","env":{"initial":{"trees":[{"displayName":"proj"}]}},"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]},{"role":"assistant","content":[{"type":"text","text":"hi"}]}]}`

	ampPath := filepath.Join(
		ampDir,
		"T-019ca26f-ffff-aaaa-bbbb-cccccccccccc.json",
	)
	dbtest.WriteTestFile(t, ampPath, []byte(content))

	engine.SyncPaths([]string{ampPath})

	assertSessionState(
		t, database,
		"amp:T-019ca26f-ffff-aaaa-bbbb-cccccccccccc",
		func(sess *db.Session) {
			if sess.Agent != "amp" {
				t.Errorf(
					"agent = %q, want amp", sess.Agent,
				)
			}
		},
	)
}

func TestSyncPathsVSCodeCopilotJSONLPriority(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	dir := t.TempDir()
	vscDir := filepath.Join(dir, "vscode")
	chatDir := filepath.Join(
		vscDir, "workspaceStorage", "abc123",
		"chatSessions",
	)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCodeCopilot: {vscDir},
		},
		Machine: "local",
	})

	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	session := fmt.Sprintf(
		`{"version":1,"sessionId":"%s",`+
			`"creationDate":1704103200000,`+
			`"lastMessageDate":1704103260000,`+
			`"requests":[{"requestId":"r1",`+
			`"message":{"text":"hello"},`+
			`"response":[{"value":"hi"}],`+
			`"timestamp":1704103200000}]}`,
		uuid,
	)

	jsonPath := filepath.Join(chatDir, uuid+".json")
	jsonlPath := filepath.Join(chatDir, uuid+".jsonl")
	dbtest.WriteTestFile(t, jsonPath, []byte(session))
	dbtest.WriteTestFile(
		t, jsonlPath,
		[]byte(`{"kind":0,"v":`+session+`}`),
	)

	// Sync the .json path; classifier should skip it
	// because a .jsonl sibling exists.
	engine.SyncPaths([]string{jsonPath})

	ctx := context.Background()
	page, err := database.ListSessions(
		ctx, db.SessionFilter{Limit: 10},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 0 {
		t.Errorf(
			"expected 0 sessions (.json skipped), got %d",
			len(page.Sessions),
		)
	}
}

func TestPiSessionIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Build a temp pi session directory:
	//   <piDir>/<encoded-cwd>/<session-id>.jsonl
	piDir := t.TempDir()
	cwdSubdir := filepath.Join(piDir, "--Users-alice-code-my-project")
	if err := os.MkdirAll(cwdSubdir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Use the existing pi session fixture from the parser testdata.
	// The fixture has id="pi-test-session-uuid".
	_, callerFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(
		filepath.Dir(callerFile), "..", "parser", "testdata", "pi",
	)
	fixtureContent, err := os.ReadFile(
		filepath.Join(fixtureDir, "session.jsonl"),
	)
	if err != nil {
		t.Fatalf("reading pi fixture: %v", err)
	}

	sessionFile := filepath.Join(cwdSubdir, "pi-test-session-uuid.jsonl")
	dbtest.WriteTestFile(t, sessionFile, fixtureContent)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPi: {piDir},
		},
		Machine: "local",
	})

	stats := engine.SyncAll(nil)
	if stats.Synced != 1 {
		t.Fatalf("expected 1 synced session, got %d (failed=%d)",
			stats.Synced, stats.Failed)
	}

	assertSessionState(t, database, "pi:pi-test-session-uuid",
		func(sess *db.Session) {
			if sess.Agent != "pi" {
				t.Errorf("agent = %q, want %q", sess.Agent, "pi")
			}
			// The fixture has 2 real user messages. model_change and
			// compaction entries must not inflate the count after
			// postFilterCounts re-counts role="user" messages.
			if sess.UserMessageCount != 2 {
				t.Errorf("UserMessageCount = %d, want 2", sess.UserMessageCount)
			}
		},
	)

	// FindSourceFile should locate pi sessions via the "pi:" prefix.
	src := engine.FindSourceFile("pi:pi-test-session-uuid")
	if src == "" {
		t.Error("FindSourceFile returned empty for pi session")
	}

	// SyncSingleSession should work for pi sessions.
	if err := engine.SyncSingleSession("pi:pi-test-session-uuid"); err != nil {
		t.Fatalf("SyncSingleSession pi: %v", err)
	}
	assertSessionState(t, database, "pi:pi-test-session-uuid",
		func(sess *db.Session) {
			if sess.Agent != "pi" {
				t.Errorf("after SyncSingleSession: agent = %q, want %q", sess.Agent, "pi")
			}
		},
	)
}
