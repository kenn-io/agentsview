package sync_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testEnv struct {
	claudeDir         string
	codexDir          string
	cursorDir         string
	geminiDir         string
	opencodeDir       string
	kiloDir           string
	mimocodeDir       string
	forgeDir          string
	piebaldDir        string
	warpDir           string
	iflowDir          string
	ampDir            string
	piDir             string
	ompDir            string
	kiroDir           string
	shelleyDir        string
	antigravityCLIDir string
	db                *db.DB
	engine            *sync.Engine
}

type testEnvOpts struct {
	claudeDirs   []string
	codexDirs    []string
	cursorDirs   []string
	opencodeDirs []string
	kiloDirs     []string
	kiroDirs     []string
	emitter      sync.Emitter
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

func WithOpenCodeDirs(dirs []string) TestEnvOption {
	return func(o *testEnvOpts) {
		o.opencodeDirs = dirs
	}
}

func WithKiloDirs(dirs []string) TestEnvOption {
	return func(o *testEnvOpts) {
		o.kiloDirs = dirs
	}
}

func WithKiroDirs(dirs []string) TestEnvOption {
	return func(o *testEnvOpts) {
		o.kiroDirs = dirs
	}
}

func WithEmitter(em sync.Emitter) TestEnvOption {
	return func(o *testEnvOpts) {
		o.emitter = em
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
		geminiDir:         t.TempDir(),
		mimocodeDir:       t.TempDir(),
		forgeDir:          t.TempDir(),
		piebaldDir:        t.TempDir(),
		warpDir:           t.TempDir(),
		iflowDir:          t.TempDir(),
		ampDir:            t.TempDir(),
		piDir:             t.TempDir(),
		ompDir:            t.TempDir(),
		shelleyDir:        t.TempDir(),
		antigravityCLIDir: t.TempDir(),
		db:                dbtest.OpenTestDB(t),
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

	opencodeDirs := options.opencodeDirs
	if len(opencodeDirs) == 0 {
		env.opencodeDir = t.TempDir()
		opencodeDirs = []string{env.opencodeDir}
	} else {
		env.opencodeDir = opencodeDirs[0]
	}

	kiloDirs := options.kiloDirs
	if len(kiloDirs) == 0 {
		env.kiloDir = t.TempDir()
		kiloDirs = []string{env.kiloDir}
	} else {
		env.kiloDir = kiloDirs[0]
	}

	kiroDirs := options.kiroDirs
	if len(kiroDirs) == 0 {
		env.kiroDir = t.TempDir()
		kiroDirs = []string{env.kiroDir}
	} else {
		env.kiroDir = kiroDirs[0]
	}

	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude:         claudeDirs,
			parser.AgentCodex:          codexDirs,
			parser.AgentCursor:         cursorDirs,
			parser.AgentGemini:         {env.geminiDir},
			parser.AgentOpenCode:       opencodeDirs,
			parser.AgentKilo:           kiloDirs,
			parser.AgentMiMoCode:       {env.mimocodeDir},
			parser.AgentForge:          {env.forgeDir},
			parser.AgentPiebald:        {env.piebaldDir},
			parser.AgentWarp:           {env.warpDir},
			parser.AgentIflow:          {env.iflowDir},
			parser.AgentAmp:            {env.ampDir},
			parser.AgentPi:             {env.piDir},
			parser.AgentOMP:            {env.ompDir},
			parser.AgentKiro:           kiroDirs,
			parser.AgentShelley:        {env.shelleyDir},
			parser.AgentAntigravityCLI: {env.antigravityCLIDir},
		},
		Machine: "local",
		Emitter: options.emitter,
	})
	return env
}

func setupFocusedTestEnv(t *testing.T, agents ...parser.AgentType) *testEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	env := &testEnv{db: dbtest.OpenTestDB(t)}
	agentDirs := make(map[parser.AgentType][]string, len(agents))
	for _, agent := range agents {
		dir := t.TempDir()
		assignFocusedAgentDir(t, env, agent, dir)
		agentDirs[agent] = []string{dir}
	}

	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: agentDirs,
		Machine:   "local",
	})
	return env
}

func setupSingleAgentTestEnv(t *testing.T, agent parser.AgentType) *testEnv {
	t.Helper()
	return setupFocusedTestEnv(t, agent)
}

func setupSingleAgentTestEnvWithDirs(
	t *testing.T, agent parser.AgentType, dirs []string,
) *testEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	require.NotEmpty(t, dirs, "focused fixture dirs")

	env := &testEnv{db: dbtest.OpenTestDB(t)}
	assignFocusedAgentDir(t, env, agent, dirs[0])
	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			agent: dirs,
		},
		Machine: "local",
	})
	return env
}

func assignFocusedAgentDir(
	t *testing.T, env *testEnv, agent parser.AgentType, dir string,
) {
	t.Helper()
	switch agent {
	case parser.AgentClaude:
		env.claudeDir = dir
	case parser.AgentCodex:
		env.codexDir = dir
	case parser.AgentOpenCode:
		env.opencodeDir = dir
	case parser.AgentGemini:
		env.geminiDir = dir
	case parser.AgentKilo:
		env.kiloDir = dir
	case parser.AgentMiMoCode:
		env.mimocodeDir = dir
	case parser.AgentPiebald:
		env.piebaldDir = dir
	case parser.AgentForge:
		env.forgeDir = dir
	case parser.AgentWarp:
		env.warpDir = dir
	case parser.AgentPi:
		env.piDir = dir
	case parser.AgentOMP:
		env.ompDir = dir
	case parser.AgentKiro:
		env.kiroDir = dir
	case parser.AgentShelley:
		env.shelleyDir = dir
	case parser.AgentAntigravityCLI:
		env.antigravityCLIDir = dir
	default:
		t.Fatalf("unsupported focused test fixture for %s", agent)
	}
}

func TestSyncEngineKiroSQLiteUpdatePaths(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	ks := createKiroSQLiteDB(t, env.kiroDir)

	const (
		createdAt = int64(1779012000000)
		updatedAt = int64(1779012030000)
	)
	standardPayload := readKiroSQLiteFixture(t, "standard_payload.json")
	overlapPayload := readKiroSQLiteFixture(t, "overlap_payload.json")
	malformedPayload := readKiroSQLiteFixture(t, "malformed_payload.txt")

	for _, id := range []string{
		"full-sync-session",
		"physical-path-session",
		"virtual-path-session",
		"malformed-session",
	} {
		ks.addSession(
			t, "/home/user/code/kiro-app", id,
			standardPayload, createdAt, updatedAt,
		)
	}

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 4, Synced: 4, Skipped: 0,
	})
	assertSessionProject(t, env.db, "kiro:full-sync-session", "kiro_app")
	assertSessionMessageCount(t, env.db, "kiro:full-sync-session", 4)
	source := env.engine.FindSourceFile("kiro:full-sync-session")
	want := filepath.Join(env.kiroDir, "data.sqlite3") + "#full-sync-session"
	require.Equal(t, want, source)
	got, wantMtime := env.engine.SourceMtime("kiro:full-sync-session"), updatedAt*1_000_000
	require.Equal(t, wantMtime, got)
	assertMessageContent(t, env.db, "kiro:physical-path-session",
		"Build the Kiro parser",
		"I can do that.",
		"Read the source first",
		"[Other: execute_bash]",
	)
	assertMessageContent(t, env.db, "kiro:virtual-path-session",
		"Build the Kiro parser",
		"I can do that.",
		"Read the source first",
		"[Other: execute_bash]",
	)

	// A second full sync with the Kiro DB unchanged must not rewrite the row
	// (Synced stays 0). After the initial fan-out, unchanged Kiro SQLite
	// sources are accounted as the single provider DB path.
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 0, Skipped: 0,
	})

	ks.updateSession(t, "full-sync-session", overlapPayload, 1779015610000)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1, Skipped: 0,
	})
	assertSessionMessageCount(t, env.db, "kiro:full-sync-session", 2)

	ks.updateSession(t, "physical-path-session", overlapPayload, 1779015620000)
	env.engine.SyncPaths([]string{ks.path})

	assertSessionMessageCount(t, env.db, "kiro:physical-path-session", 2)
	assertMessageContent(t, env.db, "kiro:physical-path-session",
		"Current store should win",
		"Using the SQLite version.",
	)

	ks.updateSession(t, "virtual-path-session", overlapPayload, 1779015630000)
	env.engine.SyncPaths([]string{
		parser.KiroSQLiteVirtualPath(ks.path, "virtual-path-session"),
	})

	assertSessionMessageCount(t, env.db, "kiro:virtual-path-session", 2)
	assertMessageContent(t, env.db, "kiro:virtual-path-session",
		"Current store should win",
		"Using the SQLite version.",
	)

	ks.updateSession(t, "malformed-session", malformedPayload, 1779015640000)
	// Kiro is provider-authoritative: the database is rediscovered and
	// re-parsed (TotalSessions counts the source), but the malformed payload
	// yields no parseable session, so nothing is written and the previously
	// archived session is preserved.
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 0, Skipped: 0,
	})
	assertSessionMessageCount(t, env.db, "kiro:malformed-session", 4)
}

func TestSyncEngineKiroSQLiteCurrentStoreShadowsLegacy(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	ks := createKiroSQLiteDB(t, env.kiroDir)
	ks.addSession(
		t, "/home/user/code/current-kiro", "overlap-session",
		readKiroSQLiteFixture(t, "overlap_payload.json"),
		1779015600000, 1779015610000,
	)
	writeLegacyKiroSession(
		t, env.kiroDir, "overlap-session",
		"legacy should not win",
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1, Skipped: 0,
	})
	assertSessionProject(t, env.db, "kiro:overlap-session", "current_kiro")

	// Kiro is provider-authoritative: the current-store database is
	// rediscovered and re-parsed on every full sync, but an unchanged row is
	// dropped from the write batch (Synced stays 0) instead of being rewritten
	// and recounted. The legacy file stays shadowed throughout.
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 0, Skipped: 0,
	})
	sess, err := env.db.GetSessionFull(
		context.Background(), "kiro:overlap-session",
	)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, sess, "expected sqlite-backed session")
	require.NotNil(t, sess.FilePath, "expected sqlite-backed session")
	require.Contains(t, *sess.FilePath, "data.sqlite3#overlap-session", "expected sqlite-backed session, got %+v", sess)

	legacyPath := filepath.Join(env.kiroDir, "overlap-session.jsonl")
	env.engine.SyncPaths([]string{legacyPath})
	sess, err = env.db.GetSessionFull(
		context.Background(), "kiro:overlap-session",
	)
	require.NoError(t, err, "GetSessionFull after legacy event")
	require.NotNil(t, sess, "legacy event replaced sqlite-backed session")
	require.NotNil(t, sess.FilePath, "legacy event replaced sqlite-backed session")
	require.Contains(t, *sess.FilePath, "data.sqlite3#overlap-session", "legacy event replaced sqlite-backed session: %+v", sess)
}

func TestSyncRootsSinceKiroLegacyShadowedBySQLiteOutsideScope(t *testing.T) {
	legacyRoot := t.TempDir()
	sqliteRoot := t.TempDir()
	env := setupSingleAgentTestEnvWithDirs(
		t, parser.AgentKiro, []string{legacyRoot, sqliteRoot},
	)

	ks := createKiroSQLiteDB(t, sqliteRoot)
	ks.addSession(
		t, "/home/user/code/current-kiro", "overlap-session",
		readKiroSQLiteFixture(t, "overlap_payload.json"),
		1779015600000, 1779015610000,
	)
	writeLegacyKiroSession(
		t, legacyRoot, "overlap-session",
		"legacy should not be imported",
	)

	stats := env.engine.SyncRootsSince(
		context.Background(), []string{legacyRoot}, time.Time{}, nil,
	)
	assert.Equal(t, 0, stats.TotalSessions, "total sessions")
}

func TestSyncEngineKiroLegacyOnlySyncPath(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	writeLegacyKiroSession(
		t, env.kiroDir, "legacy-only-session",
		"legacy-only should sync",
	)

	legacyPath := filepath.Join(env.kiroDir, "legacy-only-session.jsonl")
	env.engine.SyncPaths([]string{legacyPath})
	assertSessionProject(
		t, env.db, "kiro:legacy-only-session", "legacy_kiro",
	)
	assertSessionMessageCount(t, env.db, "kiro:legacy-only-session", 2)
}

type fakeEmitter struct {
	mu     gosync.Mutex
	scopes []string
}

func (f *fakeEmitter) Emit(scope string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopes = append(f.scopes, scope)
}

func (f *fakeEmitter) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.scopes))
	copy(out, f.scopes)
	return out
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
	projName := strings.NewReplacer("/", "-", "\\", "-", ":", "-").
		Replace(dirPath)
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

// writeNestedCursorSession creates a Cursor transcript file under
// the nested layout <project>/agent-transcripts/<session>/<session><ext>.
func (e *testEnv) writeNestedCursorSession(
	t *testing.T, cursorDir, project, sessionID, ext,
	content string,
) string {
	t.Helper()
	return e.writeSession(
		t, cursorDir,
		filepath.Join(
			project, "agent-transcripts", sessionID,
			sessionID+ext,
		),
		content,
	)
}

func TestSyncEngineIntegration(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

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
	assert.NotEmpty(t, src, "FindSourceFile returned empty")
}

func TestSyncEngineWorktreesShareProject(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	root := t.TempDir()
	mainRepo := filepath.Join(root, "agentsview")
	worktree := filepath.Join(root, "agentsview-worktree-tool-call-arguments")
	worktreeGitDir := filepath.Join(mainRepo, ".git", "worktrees", "feature")

	dbtest.WriteTestFile(t, filepath.Join(worktree, ".git"),
		[]byte("gitdir: "+worktreeGitDir+"\n"))
	dbtest.WriteTestFile(t, filepath.Join(worktreeGitDir, "commondir"),
		[]byte("../..\n"))

	// Create a standard main repository marker.
	require.NoError(t, os.MkdirAll(filepath.Join(mainRepo, ".git"), 0o755), "mkdir main .git")

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

	projects, err := env.db.GetProjects(context.Background(), false, false)
	require.NoError(t, err, "GetProjects")
	require.Equal(t, 1, len(projects), "len(projects) = %d, want 1", len(projects))
	require.Equal(t, "agentsview", projects[0].Name, "project name = %q, want %q", projects[0].Name, "agentsview")
	require.Equal(t, 2, projects[0].SessionCount, "session_count = %d, want 2", projects[0].SessionCount)
}

func TestSyncEngineWorktreeProjectWhenPathMissing(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

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

func TestSyncEngineAppliesWorktreeProjectMapping(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	assert.Equal(t, "local", env.engine.Machine())

	root := t.TempDir()
	worktreePrefix := filepath.Join(root, "my-app.worktrees")
	sessionCwd := filepath.Join(worktreePrefix, "feature-login")
	_, err := env.db.CreateWorktreeProjectMapping(
		context.Background(),
		db.WorktreeProjectMapping{
			Machine:    "local",
			PathPrefix: worktreePrefix,
			Project:    "canonical-app",
			Enabled:    true,
		},
	)
	require.NoError(t, err, "CreateWorktreeProjectMapping")

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Worktree mapped", sessionCwd).
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()

	env.writeClaudeSessionForProject(
		t, sessionCwd,
		"mapped-worktree.jsonl", content,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	assertSessionProject(
		t, env.db, "mapped-worktree", "canonical_app",
	)
}

func TestSyncSingleSessionAppliesWorktreeProjectMapping(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	root := t.TempDir()
	worktreePrefix := filepath.Join(root, "my-app.worktrees")
	sessionCwd := filepath.Join(worktreePrefix, "feature-login")
	_, err := env.db.CreateWorktreeProjectMapping(
		context.Background(),
		db.WorktreeProjectMapping{
			Machine:    "local",
			PathPrefix: worktreePrefix,
			Project:    "canonical-app",
			Enabled:    true,
		},
	)
	require.NoError(t, err, "CreateWorktreeProjectMapping")

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Worktree mapped single", sessionCwd).
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()

	env.writeClaudeSessionForProject(
		t, sessionCwd,
		"mapped-worktree-single.jsonl", content,
	)

	err = env.engine.SyncSingleSession(
		"mapped-worktree-single",
	)
	require.NoError(t, err, "SyncSingleSession")

	assertSessionProject(
		t, env.db, "mapped-worktree-single", "canonical_app",
	)
}

func TestSyncSingleSessionSkippedClaudeDoesNotApplyWorktreeProjectMapping(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	root := t.TempDir()
	worktreePrefix := filepath.Join(root, "my-app.worktrees")
	sessionCwd := filepath.Join(worktreePrefix, "feature-login")
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Worktree mapped after initial sync", sessionCwd).
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()

	env.writeClaudeSessionForProject(
		t, sessionCwd,
		"mapped-worktree-single-skip.jsonl", content,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	before, err := env.db.GetSession(
		context.Background(), "mapped-worktree-single-skip",
	)
	require.NoError(t, err, "GetSession before mapping")
	require.NotNil(t, before, "session missing before mapping")
	require.NotEqual(t, "canonical_app", before.Project, "project before mapping = %q, want stale project", before.Project)
	require.Nil(t, before.LocalModifiedAt, "local_modified_at before mapping = %v, want nil", before.LocalModifiedAt)

	_, err = env.db.CreateWorktreeProjectMapping(
		context.Background(),
		db.WorktreeProjectMapping{
			Machine:    "local",
			PathPrefix: worktreePrefix,
			Project:    "canonical-app",
			Enabled:    true,
		},
	)
	require.NoError(t, err, "CreateWorktreeProjectMapping")

	err = env.engine.SyncSingleSession(
		"mapped-worktree-single-skip",
	)
	require.NoError(t, err, "SyncSingleSession")

	after, err := env.db.GetSession(
		context.Background(), "mapped-worktree-single-skip",
	)
	require.NoError(t, err, "GetSession after skipped sync")
	require.NotNil(t, after, "session missing after skipped sync")
	require.Equal(t, before.Project, after.Project, "project after skipped sync = %q, want %q", after.Project, before.Project)
}

func TestSyncAllSkippedClaudeDoesNotApplyWorktreeProjectMapping(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	root := t.TempDir()
	worktreePrefix := filepath.Join(root, "my-app.worktrees")
	sessionCwd := filepath.Join(worktreePrefix, "feature-login")
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Worktree mapped during normal skip", sessionCwd).
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()

	env.writeClaudeSessionForProject(
		t, sessionCwd,
		"mapped-worktree-syncall-skip.jsonl", content,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	before, err := env.db.GetSession(
		context.Background(), "mapped-worktree-syncall-skip",
	)
	require.NoError(t, err, "GetSession before mapping")
	require.NotNil(t, before, "session missing before mapping")
	require.NotEqual(t, "canonical_app", before.Project, "project before mapping = %q, want stale project", before.Project)
	_, err = env.db.CreateWorktreeProjectMapping(
		context.Background(),
		db.WorktreeProjectMapping{
			Machine:    "local",
			PathPrefix: worktreePrefix,
			Project:    "canonical-app",
			Enabled:    true,
		},
	)
	require.NoError(t, err, "CreateWorktreeProjectMapping")

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        0,
		Skipped:       1,
	})

	after, err := env.db.GetSession(
		context.Background(), "mapped-worktree-syncall-skip",
	)
	require.NoError(t, err, "GetSession after skipped sync")
	require.NotNil(t, after, "session missing after skipped sync")
	require.Equal(t, before.Project, after.Project, "project after skipped sync = %q, want %q", after.Project, before.Project)
}

func TestSyncPathsSkippedClaudeDoesNotApplyWorktreeProjectMapping(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	root := t.TempDir()
	worktreePrefix := filepath.Join(root, "my-app.worktrees")
	sessionCwd := filepath.Join(worktreePrefix, "feature-login")
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Worktree mapped during path skip", sessionCwd).
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()

	path := env.writeClaudeSessionForProject(
		t, sessionCwd,
		"mapped-worktree-syncpaths-skip.jsonl", content,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	before, err := env.db.GetSession(
		context.Background(), "mapped-worktree-syncpaths-skip",
	)
	require.NoError(t, err, "GetSession before mapping")
	require.NotNil(t, before, "session missing before mapping")
	require.NotEqual(t, "canonical_app", before.Project, "project before mapping = %q, want stale project", before.Project)
	beforeFull, err := env.db.GetSessionFull(
		context.Background(), "mapped-worktree-syncpaths-skip",
	)
	require.NoError(t, err, "GetSessionFull before mapping")

	_, err = env.db.CreateWorktreeProjectMapping(
		context.Background(),
		db.WorktreeProjectMapping{
			Machine:    "local",
			PathPrefix: worktreePrefix,
			Project:    "canonical-app",
			Enabled:    true,
		},
	)
	require.NoError(t, err, "CreateWorktreeProjectMapping")

	env.engine.SyncPaths([]string{path})

	after, err := env.db.GetSessionFull(
		context.Background(),
		"mapped-worktree-syncpaths-skip",
	)
	require.NoError(t, err, "GetSessionFull after skipped path sync")
	require.Equal(t, beforeFull.Project, after.Project, "project after skipped path sync = %q, want %q", after.Project, beforeFull.Project)
	require.Equal(t,
		testStringPtrValue(beforeFull.LocalModifiedAt),
		testStringPtrValue(after.LocalModifiedAt),
		"local_modified_at after skipped path sync = %v, want %v",
		after.LocalModifiedAt,
		beforeFull.LocalModifiedAt,
	)
}

func TestSyncSingleSessionIncrementalAppliesWorktreeProjectMapping(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	root := t.TempDir()
	worktreePrefix := filepath.Join(root, "my-app.worktrees")
	sessionCwd := filepath.Join(worktreePrefix, "feature-login")
	initial := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Worktree mapped after append", sessionCwd).
		String()

	path := env.writeClaudeSessionForProject(
		t, sessionCwd,
		"mapped-worktree-single-incremental.jsonl", initial,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	before, err := env.db.GetSession(
		context.Background(), "mapped-worktree-single-incremental",
	)
	require.NoError(t, err, "GetSession before mapping")
	require.NotNil(t, before, "session missing before mapping")
	require.NotEqual(t, "canonical_app", before.Project, "project before mapping = %q, want stale project", before.Project)
	_, err = env.db.CreateWorktreeProjectMapping(
		context.Background(),
		db.WorktreeProjectMapping{
			Machine:    "local",
			PathPrefix: worktreePrefix,
			Project:    "canonical-app",
			Enabled:    true,
		},
	)
	require.NoError(t, err, "CreateWorktreeProjectMapping")

	appended := initial + testjsonl.NewSessionBuilder().
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()
	require.NoError(t, os.WriteFile(path, []byte(appended), 0o644), "WriteFile")

	err = env.engine.SyncSingleSession(
		"mapped-worktree-single-incremental",
	)
	require.NoError(t, err, "SyncSingleSession")

	assertSessionMessageCount(
		t, env.db, "mapped-worktree-single-incremental", 2,
	)
	assertSessionProject(
		t, env.db, "mapped-worktree-single-incremental", "canonical_app",
	)
}

func TestSyncAllIncrementalAppliesWorktreeProjectMapping(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	root := t.TempDir()
	worktreePrefix := filepath.Join(root, "my-app.worktrees")
	sessionCwd := filepath.Join(worktreePrefix, "feature-login")
	initial := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Worktree mapped during normal append", sessionCwd).
		String()

	path := env.writeClaudeSessionForProject(
		t, sessionCwd,
		"mapped-worktree-syncall-incremental.jsonl", initial,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	before, err := env.db.GetSession(
		context.Background(), "mapped-worktree-syncall-incremental",
	)
	require.NoError(t, err, "GetSession before mapping")
	require.NotNil(t, before, "session missing before mapping")
	require.NotEqual(t, "canonical_app", before.Project, "project before mapping = %q, want stale project", before.Project)
	_, err = env.db.CreateWorktreeProjectMapping(
		context.Background(),
		db.WorktreeProjectMapping{
			Machine:    "local",
			PathPrefix: worktreePrefix,
			Project:    "canonical-app",
			Enabled:    true,
		},
	)
	require.NoError(t, err, "CreateWorktreeProjectMapping")

	appended := initial + testjsonl.NewSessionBuilder().
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()
	require.NoError(t, os.WriteFile(path, []byte(appended), 0o644), "WriteFile")

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	assertSessionMessageCount(
		t, env.db, "mapped-worktree-syncall-incremental", 2,
	)
	assertSessionProject(
		t, env.db, "mapped-worktree-syncall-incremental", "canonical_app",
	)
}

func TestResyncAllAppliesWorktreeProjectMappingDuringBulkWrites(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	root := t.TempDir()
	worktreePrefix := filepath.Join(root, "my-app.worktrees")
	sessionCwd := filepath.Join(worktreePrefix, "feature-login")
	_, err := env.db.CreateWorktreeProjectMapping(
		context.Background(),
		db.WorktreeProjectMapping{
			Machine:    "local",
			PathPrefix: worktreePrefix,
			Project:    "canonical-app",
			Enabled:    true,
		},
	)
	require.NoError(t, err, "CreateWorktreeProjectMapping")

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Worktree mapped by resync", sessionCwd).
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()

	env.writeClaudeSessionForProject(
		t, sessionCwd,
		"mapped-worktree-resync.jsonl", content,
	)

	stats := env.engine.ResyncAll(context.Background(), nil)
	require.False(t, stats.Aborted, "ResyncAll aborted: %+v", stats)
	require.Equal(t, 1, stats.Synced, "ResyncAll synced = %d, want 1: %+v", stats.Synced, stats)

	assertSessionProject(
		t, env.db, "mapped-worktree-resync", "canonical_app",
	)
}

func TestResyncAllExcludesExistingClaudeUsageProbe(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	const sessionID = "usage-probe"
	usageCmd := "<command-name>/usage</command-name>\n" +
		"            <command-message>usage</command-message>\n" +
		"            <command-args></command-args>"
	content := testjsonl.ClaudeUserJSON(usageCmd, tsEarly)
	path := env.writeClaudeSession(
		t, "ClaudeProbe", sessionID+".jsonl", content,
	)

	size := int64(len(content))
	mtime := time.Now().Add(-time.Hour).UnixNano()
	firstMessage := "/usage"
	startedAt := tsEarly
	require.NoError(t, env.db.UpsertSession(db.Session{
		ID:               sessionID,
		Project:          "ClaudeProbe",
		Machine:          "local",
		Agent:            string(parser.AgentClaude),
		FirstMessage:     &firstMessage,
		StartedAt:        &startedAt,
		EndedAt:          &startedAt,
		MessageCount:     1,
		UserMessageCount: 1,
		FilePath:         &path,
		FileSize:         &size,
		FileMtime:        &mtime,
	}), "seed stale /usage probe row")
	require.NoError(t, env.db.SetSessionDataVersion(
		sessionID, db.CurrentDataVersion()-1,
	), "seed stale data_version")

	stats := env.engine.ResyncAll(context.Background(), nil)
	require.False(t, stats.Aborted, "ResyncAll aborted: %+v", stats)
	assert.Equal(t, 0, stats.OrphanedCopied,
		"usage probe must not be copied back as an orphan")

	got, err := env.db.GetSession(context.Background(), sessionID)
	require.NoError(t, err, "GetSession after ResyncAll")
	assert.Nil(t, got, "stale /usage probe row must be excluded")
	assert.False(t, env.db.IsSessionExcluded(sessionID),
		"parser exclusions must not become permanent user deletions")
}

func TestSyncEngineCodex(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)

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
		assert.Equal(t, "codex", sess.Agent, "agent = %q", sess.Agent)
	})
}

func TestSyncEngineProgress(t *testing.T) {
	env := setupFocusedTestEnv(t, parser.AgentClaude, parser.AgentPiebald)

	msg := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "msg").
		String()

	var firstClaudePath string
	for _, name := range []string{"a", "b", "c"} {
		path := env.writeClaudeSession(
			t, "test-proj", name+".jsonl", msg,
		)
		if firstClaudePath == "" {
			firstClaudePath = path
		}
	}
	piebald := createPiebaldDB(t, env.piebaldDir)
	piebald.addChat(t, 42, "Piebald", "Prompt.", "Answer.", "2026-05-01T10:05:00Z")

	var progressCalls int
	var firstTotal int
	var last sync.Progress
	var events []sync.Progress
	var seenCurrent sync.Progress
	env.engine.SyncAll(context.Background(), func(p sync.Progress) {
		progressCalls++
		if firstTotal == 0 {
			firstTotal = p.SessionsTotal
		}
		last = p
		events = append(events, p)
		if seenCurrent.Phase == "" {
			current, ok := env.engine.CurrentProgress()
			require.True(t, ok, "CurrentProgress should be available during sync")
			assert.Equal(t, p.Phase, current.Phase, "current progress phase")
			assert.Equal(t, p.SessionsTotal, current.SessionsTotal, "current progress total")
			seenCurrent = current
		}
	})

	assert.NotZero(t, progressCalls, "expected progress callbacks")
	assert.Equal(t, 4, firstTotal, "first progress total = %d, want 4", firstTotal)
	assert.Equal(t, 4, last.SessionsDone, "last progress = %d/%d, want 4/4", last.SessionsDone, last.SessionsTotal)
	assert.Equal(t, 4, last.SessionsTotal, "last progress = %d/%d, want 4/4", last.SessionsDone, last.SessionsTotal)
	requireProgressDoneOnce(t, events, 4)
	require.NotEmpty(t, seenCurrent.Phase, "expected progress to be observed")
	_, ok := env.engine.CurrentProgress()
	assert.False(t, ok, "CurrentProgress should be cleared after sync")
	requireDiscoveryBeforeSyncing(t, events, false)

	progressCalls = 0
	firstTotal = 0
	last = sync.Progress{}
	events = nil
	env.engine.SyncAll(context.Background(), func(p sync.Progress) {
		progressCalls++
		if firstTotal == 0 {
			firstTotal = p.SessionsTotal
		}
		last = p
		events = append(events, p)
	})
	assert.NotZero(t, progressCalls, "expected progress callbacks on second sync")
	assert.Equal(t, 4, firstTotal, "second first progress total = %d, want 4", firstTotal)
	assert.Equal(t, 4, last.SessionsDone, "second last progress = %d/%d, want 4/4", last.SessionsDone, last.SessionsTotal)
	assert.Equal(t, 4, last.SessionsTotal, "second last progress = %d/%d, want 4/4", last.SessionsDone, last.SessionsTotal)
	requireProgressDoneOnce(t, events, 4)

	env.engine.SyncPaths([]string{firstClaudePath})
	_, ok = env.engine.CurrentProgress()
	assert.False(t, ok, "CurrentProgress should be cleared after SyncPaths")

	var resyncEvents []sync.Progress
	stats := env.engine.ResyncAll(context.Background(), func(p sync.Progress) {
		resyncEvents = append(resyncEvents, p)
	})
	require.False(t, stats.Aborted, "resync aborted: %+v", stats.Warnings)

	if env.db.HasFTS() {
		var fts sync.Progress
		for _, event := range resyncEvents {
			if event.Phase == sync.PhaseRebuildingSearch {
				fts = event
				break
			}
		}
		require.Equal(t, sync.PhaseRebuildingSearch, fts.Phase, "missing FTS rebuild progress event; events=%+v", resyncEvents)
		assert.True(t, fts.Resync, "FTS progress should identify full resync")
		assert.Contains(t, fts.Detail, "Rebuilding search index")
		assert.Contains(t, fts.Hint, "may take a while")
	}

	requireDiscoveryBeforeSyncing(t, resyncEvents, true)
}

func requireProgressDoneOnce(t *testing.T, events []sync.Progress, wantTotal int) {
	t.Helper()
	require.NotEmpty(t, events, "expected progress callbacks")
	var doneCount int
	var firstDoneIdx = -1
	for i, e := range events {
		if e.Phase == sync.PhaseDone {
			doneCount++
			if firstDoneIdx == -1 {
				firstDoneIdx = i
			}
		}
	}
	require.Equal(t, 1, doneCount, "PhaseDone emitted %d times, want exactly 1; events=%+v", doneCount, events)
	require.Equal(t, len(events)-1, firstDoneIdx, "PhaseDone at index %d, want last event (index %d)", firstDoneIdx, len(events)-1)
	last := events[len(events)-1]
	require.Equal(t, last.SessionsTotal, last.SessionsDone, "final progress = %d/%d, want done", last.SessionsDone, last.SessionsTotal)
	require.Equal(t, wantTotal, last.SessionsTotal, "final progress = %d/%d, want %d/%d", last.SessionsDone, last.SessionsTotal, wantTotal, wantTotal)
	var peakMessages int
	for _, e := range events {
		if e.MessagesIndexed > peakMessages {
			peakMessages = e.MessagesIndexed
		}
	}
	require.GreaterOrEqual(t, last.MessagesIndexed, peakMessages,
		"final MessagesIndexed = %d regressed from peak %d", last.MessagesIndexed, peakMessages)
}

func requireDiscoveryBeforeSyncing(t *testing.T, events []sync.Progress, wantResync bool) {
	t.Helper()
	discoveryIdx, syncingIdx := -1, -1
	for i, event := range events {
		if event.Phase == sync.PhaseDiscovering && discoveryIdx == -1 {
			discoveryIdx = i
		}
		if event.Phase == sync.PhaseSyncing && syncingIdx == -1 {
			syncingIdx = i
		}
	}
	require.NotEqual(t, -1, discoveryIdx,
		"missing discovery progress event; events=%+v", events)
	require.NotEqual(t, -1, syncingIdx,
		"missing syncing progress event; events=%+v", events)
	assert.Less(t, discoveryIdx, syncingIdx,
		"discovery must be reported before syncing")
	assert.Equal(t, wantResync, events[discoveryIdx].Resync,
		"discovery progress resync flag")
	assert.Contains(t, events[discoveryIdx].Detail, "Discovering sessions")
}

func TestSyncEngineHashSkip(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

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
	require.True(t, ok, "file info not stored")
	require.NotZero(t, mtime, "mtime not stored")
	require.NotZero(t, size, "size not stored")

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

func TestSyncAllDedupesClaudeSourcesBySessionID(t *testing.T) {
	liveDir := t.TempDir()
	archiveDir := t.TempDir()
	env := setupTestEnv(t, WithClaudeDirs([]string{liveDir, archiveDir}))

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "same logical session").
		String()
	livePath := env.writeSession(
		t, liveDir, filepath.Join("proj-live", "duplicate.jsonl"), content,
	)
	archivePath := env.writeSession(
		t, archiveDir, filepath.Join("proj-archive", "duplicate.jsonl"), content,
	)
	older := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Second)
	require.NoError(t, os.Chtimes(archivePath, older, older))
	require.NoError(t, os.Chtimes(livePath, newer, newer))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	liveInfo, err := os.Stat(livePath)
	require.NoError(t, err)
	storedSize, storedMtime, ok := env.db.GetSessionFileInfo("duplicate")
	require.True(t, ok, "file info not stored")
	assert.Equal(t, liveInfo.Size(), storedSize)
	assert.Equal(t, liveInfo.ModTime().UnixNano(), storedMtime)

	touchedArchive := newer.Add(time.Second)
	require.NoError(t, os.Chtimes(archivePath, touchedArchive, touchedArchive))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        0,
		Skipped:       1,
	})
}

func TestSyncAllClaudeDuplicateLiveGrowthBeatsUnchangedStoredArchive(t *testing.T) {
	liveDir := t.TempDir()
	archiveDir := t.TempDir()
	env := setupTestEnv(t, WithClaudeDirs([]string{liveDir, archiveDir}))

	initial := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "initial message").
		String()
	livePath := env.writeSession(
		t, liveDir, filepath.Join("proj-live", "duplicate-grow.jsonl"), initial,
	)
	archivePath := env.writeSession(
		t, archiveDir, filepath.Join("proj-archive", "duplicate-grow.jsonl"), initial,
	)
	older := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Second)
	require.NoError(t, os.Chtimes(livePath, older, older))
	require.NoError(t, os.Chtimes(archivePath, newer, newer))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	assert.Equal(t, archivePath, env.db.GetSessionFilePath("duplicate-grow"))

	f, err := os.OpenFile(livePath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZeroS5, "live follow-up").
		String())
	require.NoError(t, err)
	require.NoError(t, f.Close())

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	assert.Equal(t, livePath, env.db.GetSessionFilePath("duplicate-grow"))
	assertMessageContent(t, env.db, "duplicate-grow", "initial message", "live follow-up")
}

func TestSyncAllSinceClaudeDuplicateTouchedStaleDoesNotBeatPreferred(t *testing.T) {
	liveDir := t.TempDir()
	archiveDir := t.TempDir()
	env := setupTestEnv(t, WithClaudeDirs([]string{liveDir, archiveDir}))

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "same logical session").
		String()
	livePath := env.writeSession(
		t, liveDir, filepath.Join("proj-live", "duplicate-since.jsonl"), content,
	)
	archivePath := env.writeSession(
		t, archiveDir, filepath.Join("proj-archive", "duplicate-since.jsonl"), content,
	)
	baseTime := time.Now().Add(-3 * time.Hour)
	preferredTime := baseTime.Add(time.Hour)
	require.NoError(t, os.Chtimes(archivePath, baseTime, baseTime))
	require.NoError(t, os.Chtimes(livePath, preferredTime, preferredTime))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	assert.Equal(t, livePath, env.db.GetSessionFilePath("duplicate-since"))

	cutoff := preferredTime.Add(time.Hour)
	touchedArchive := cutoff.Add(time.Minute)
	require.NoError(t, os.Chtimes(archivePath, touchedArchive, touchedArchive))

	stats := env.engine.SyncAllSince(context.Background(), cutoff, nil)
	assert.Zero(t, stats.Synced, "stale duplicate must not be re-synced")
	assert.Equal(t, livePath, env.db.GetSessionFilePath("duplicate-since"))
	assertMessageContent(t, env.db, "duplicate-since", "same logical session")
}

func TestSyncPathsClaudeDuplicateTouchedStaleDoesNotBeatPreferred(t *testing.T) {
	liveDir := t.TempDir()
	archiveDir := t.TempDir()
	env := setupTestEnv(t, WithClaudeDirs([]string{liveDir, archiveDir}))

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "same logical session").
		String()
	livePath := env.writeSession(
		t, liveDir, filepath.Join("proj-live", "duplicate-watch.jsonl"), content,
	)
	archivePath := env.writeSession(
		t, archiveDir, filepath.Join("proj-archive", "duplicate-watch.jsonl"), content,
	)
	older := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Second)
	require.NoError(t, os.Chtimes(archivePath, older, older))
	require.NoError(t, os.Chtimes(livePath, newer, newer))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	assert.Equal(t, livePath, env.db.GetSessionFilePath("duplicate-watch"))

	touchedArchive := newer.Add(time.Second)
	require.NoError(t, os.Chtimes(archivePath, touchedArchive, touchedArchive))

	env.engine.SyncPaths([]string{archivePath})
	assert.Zero(t, env.engine.LastSyncStats().Synced,
		"stale watcher duplicate must not be re-synced")
	assert.Equal(t, livePath, env.db.GetSessionFilePath("duplicate-watch"))
	assertMessageContent(t, env.db, "duplicate-watch", "same logical session")
}

func TestSyncAllClaudeDuplicatePathRewriterKeepsStoredPreferred(t *testing.T) {
	liveDir := t.TempDir()
	archiveDir := t.TempDir()
	database := dbtest.OpenTestDB(t)
	rewriter := func(path string) string {
		return "host:" + path
	}
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {liveDir, archiveDir},
		},
		Machine:      "remote-host",
		IDPrefix:     "host~",
		PathRewriter: rewriter,
	})

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "same logical session").
		String()
	livePath := filepath.Join(liveDir, "proj-live", "duplicate-remote.jsonl")
	archivePath := filepath.Join(archiveDir, "proj-archive", "duplicate-remote.jsonl")
	dbtest.WriteTestFile(t, livePath, []byte(content))
	dbtest.WriteTestFile(t, archivePath, []byte(content))
	older := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Second)
	require.NoError(t, os.Chtimes(archivePath, older, older))
	require.NoError(t, os.Chtimes(livePath, newer, newer))

	runSyncAndAssert(t, engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	assert.Equal(t, rewriter(livePath), database.GetSessionFilePath("host~duplicate-remote"))

	touchedArchive := newer.Add(time.Second)
	require.NoError(t, os.Chtimes(archivePath, touchedArchive, touchedArchive))

	runSyncAndAssert(t, engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        0,
		Skipped:       1,
	})
	assert.Equal(t, rewriter(livePath), database.GetSessionFilePath("host~duplicate-remote"))
	assertMessageContent(t, database, "host~duplicate-remote", "same logical session")
}

func TestSyncEngineSkipCache(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	// Write malformed content that produces 0 valid messages
	path := env.writeClaudeSession(
		t, "test-proj", "skip-test.jsonl",
		"not json at all\x00\x01",
	)

	// The deliberately malformed content yields one parser malformed
	// line, which the sync summary now surfaces per agent.
	malformed := sync.AnomalyStats{
		MalformedLinesByAgent: map[string]int{"claude": 1},
		MalformedLinesTotal:   1,
	}

	// First sync — file parsed (empty session stored)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1, Skipped: 0, Anomalies: malformed,
	})

	// Second sync — unchanged mtime, should be skipped
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 0 + 1, Synced: 0, Skipped: 1})

	// Touch file (change mtime) but keep same content
	time.Sleep(10 * time.Millisecond)
	os.Chtimes(path, time.Now(), time.Now())

	// Third sync — mtime changed → re-synced (harmless)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1 + 0, Synced: 1, Skipped: 0, Anomalies: malformed,
	})
}

func TestSyncEngineFileAppend(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

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
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	original := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "original question").
		AddClaudeAssistant(tsZeroS5, "original answer").
		String()

	path := env.writeClaudeSession(
		t, "test-proj", "replace-test.jsonl", original,
	)

	env.engine.SyncAll(context.Background(), nil)
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
	err := env.engine.SyncSingleSession(
		"replace-test",
	)
	require.NoError(t, err, "SyncSingleSession")

	assertMessageContent(
		t, env.db, "replace-test",
		"updated question", "updated answer",
	)
}

func TestSyncSingleSessionHash(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "hello").
		AddClaudeAssistant(tsZeroS5, "hi").
		String()

	env.writeClaudeSession(
		t, "test-proj", "single-hash.jsonl", content,
	)

	env.engine.SyncAll(context.Background(), nil)
	env.assertResyncRoundTrip(t, "single-hash")
}

func TestSyncSingleSessionHashCodex(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)

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

	env.engine.SyncAll(context.Background(), nil)
	env.assertResyncRoundTrip(t, sessionID)
}

func TestSyncAllImportsCodexExec(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)

	uuid := "e5f6a7b8-5678-9012-cdef-123456789012"
	// Exec-originated sessions should be imported during the
	// normal bulk sync path.
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

	env.engine.SyncAll(context.Background(), nil)

	assertSessionState(
		t, env.db, "codex:"+uuid,
		func(sess *db.Session) {
			assert.Equal(t, "codex", sess.Agent, "agent = %q, want codex", sess.Agent)
		},
	)
}

func TestSyncAllImportsCodexExecFromLegacySkipCache(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)

	uuid := "f6a7b8c9-6789-0123-def0-234567890123"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			tsEarly, uuid,
			"/home/user/code/api", "codex_exec",
		).
		AddCodexMessage(tsEarlyS1, "user", "run ls").
		AddCodexMessage(tsEarlyS5, "assistant", "done").
		String()

	path := env.writeCodexSession(
		t, filepath.Join("2024", "01", "15"),
		"rollout-20240115-"+uuid+".jsonl", content,
	)
	info, err := os.Stat(path)
	require.NoError(t, err, "stat codex session")

	require.NoError(t, env.db.ReplaceSkippedFiles(map[string]int64{
		path: info.ModTime().UnixNano(),
	}), "seed skipped files")

	// setupTestEnv already built an engine, which ran the
	// codex exec migration against an empty skip cache and
	// flipped the flag to "done". Reset the flag so the new
	// engine below observes a legacy skip entry and scrubs
	// it, matching the production upgrade path.
	err = env.db.SetSyncState(
		sync.CodexExecMigrationKey, "",
	)
	require.NoError(t, err, "reset migration flag")

	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude:   {env.claudeDir},
			parser.AgentCodex:    {env.codexDir},
			parser.AgentCursor:   {env.cursorDir},
			parser.AgentGemini:   {env.geminiDir},
			parser.AgentOpenCode: {env.opencodeDir},
			parser.AgentIflow:    {env.iflowDir},
			parser.AgentAmp:      {env.ampDir},
			parser.AgentPi:       {env.piDir},
		},
		Machine: "local",
	})

	env.engine.SyncAll(context.Background(), nil)

	assertSessionState(
		t, env.db, "codex:"+uuid,
		func(sess *db.Session) {
			assert.Equal(t, "codex", sess.Agent, "agent = %q, want codex", sess.Agent)
		},
	)
}

// TestCodexExecMigrationIdempotent verifies that once the
// codex exec skip cache migration has run, subsequent engine
// starts do not re-scan or remove entries — even those that
// point at codex_exec files, which legitimately get cached
// post-migration when the parser fails on them. The flag in
// pg_sync_state is the gate; without it a broken exec file
// would be reopened on every startup.
func TestCodexExecMigrationIdempotent(t *testing.T) {
	env := setupTestEnv(t)

	// setupTestEnv already built an engine that set the
	// migration flag against an empty skip cache. Write a
	// codex exec file and seed it into the skip cache to
	// mimic a fresh parse-error cache entry made by a
	// post-migration sync.
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			tsEarly, uuid,
			"/home/user/code/api", "codex_exec",
		).
		AddCodexMessage(tsEarlyS1, "user", "run ls").
		String()

	path := env.writeCodexSession(
		t, filepath.Join("2024", "01", "15"),
		"rollout-20240115-"+uuid+".jsonl", content,
	)
	info, err := os.Stat(path)
	require.NoError(t, err, "stat codex session")

	require.NoError(t, env.db.ReplaceSkippedFiles(map[string]int64{
		path: info.ModTime().UnixNano(),
	}), "seed skipped files")

	// Rebuild the engine without resetting the migration
	// flag. The migration must be a no-op: the seeded entry
	// stays in the DB and the engine respects it on sync.
	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude:   {env.claudeDir},
			parser.AgentCodex:    {env.codexDir},
			parser.AgentCursor:   {env.cursorDir},
			parser.AgentGemini:   {env.geminiDir},
			parser.AgentOpenCode: {env.opencodeDir},
			parser.AgentIflow:    {env.iflowDir},
			parser.AgentAmp:      {env.ampDir},
			parser.AgentPi:       {env.piDir},
		},
		Machine: "local",
	})

	env.engine.SyncAll(context.Background(), nil)

	loaded, err := env.db.LoadSkippedFiles()
	require.NoError(t, err, "load skipped files")
	_, ok := loaded[path]
	require.True(t, ok,
		"post-migration skip entry for %s was cleared; migration must be idempotent",
		path,
	)
}

func TestSyncEngineTombstoneClearOnMtimeChange(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	// Write something that produces 0 messages but parses OK
	path := env.writeClaudeSession(
		t, "test-proj", "tombstone-clear.jsonl", "garbage\n",
	)

	// First sync
	env.engine.SyncAll(context.Background(), nil)

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
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	// 1. Create a session in a directory "default-proj"
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "hello").
		String()

	env.writeClaudeSession(
		t, "default-proj", "fallback-test.jsonl", content,
	)

	// 2. Initial sync - should get "default-proj"
	env.engine.SyncAll(context.Background(), nil)

	assertSessionProject(t, env.db, "fallback-test", "default_proj")

	// 3. Manually update project to "custom_proj"
	// This simulates a user override we want to preserve
	env.updateSessionProject(t, "fallback-test", "custom_proj")

	assertSessionProject(t, env.db, "fallback-test", "custom_proj")

	// 4. SyncSingleSession should NOT revert to "default_proj"
	err := env.engine.SyncSingleSession("fallback-test")
	require.NoError(t, err, "SyncSingleSession")

	assertSessionProject(t, env.db, "fallback-test", "custom_proj")

	// Case A: Empty project -> should fall back to directory
	env.updateSessionProject(t, "fallback-test", "")

	err = env.engine.SyncSingleSession("fallback-test")
	require.NoError(t, err, "SyncSingleSession (empty)")

	assertSessionProject(t, env.db, "fallback-test", "default_proj")

	// Case B: Bad project -> should fall back to directory
	env.updateSessionProject(t, "fallback-test", "_Users_alice_bad")

	err = env.engine.SyncSingleSession("fallback-test")
	require.NoError(t, err, "SyncSingleSession (bad)")

	assertSessionProject(t, env.db, "fallback-test", "default_proj")
}

func TestSyncEngineNoTrailingNewline(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

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
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "Hello").
		String()
	untouchedContent := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "Untouched").
		String()

	path := env.writeClaudeSession(
		t, "test-proj", "paths-test.jsonl", content,
	)
	env.writeClaudeSession(
		t, "test-proj", "paths-untouched.jsonl", untouchedContent,
	)

	// Initial full sync
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 2 + 0, Synced: 2, Skipped: 0})

	assertSessionMessageCount(t, env.db, "paths-test", 1)
	assertSessionMessageCount(t, env.db, "paths-untouched", 1)

	// Append a message (changes size and hash)
	appended := content + testjsonl.NewSessionBuilder().
		AddClaudeAssistant(tsZeroS5, "reply").
		String()
	os.WriteFile(path, []byte(appended), 0o644)

	// SyncPaths with just the changed file
	env.engine.SyncPaths([]string{path})

	assertSessionMessageCount(t, env.db, "paths-test", 2)
	assertSessionMessageCount(t, env.db, "paths-untouched", 1)
}

func TestSyncPathsIgnoresNonSessionFiles(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

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
			assert.Equal(t, "codex", sess.Agent, "agent = %q, want codex", sess.Agent)
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
	assert.Nil(t, sess, "agent-* file should be ignored")
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
			assert.Equal(t, "gemini", sess.Agent, "agent = %q, want gemini", sess.Agent)
		},
	)
	assertSessionMessageCount(t, env.db, "gemini:"+sessionID, 2)
}

func TestSyncPathsGeminiJSONL(t *testing.T) {
	env := setupTestEnv(t)

	sessionID := "gem-test-jsonl"
	hash := "abcdef1234567890"
	content := strings.Join([]string{
		`{"sessionId":"gem-test-jsonl","projectHash":"abcdef1234567890","startTime":"` + tsEarly + `","lastUpdated":"` + tsEarly + `","kind":"main"}`,
		`{"id":"m1","timestamp":"` + tsEarly + `","type":"user","content":[{"text":"Hello Gemini"}]}`,
		`{"$set":{"lastUpdated":"` + tsEarlyS5 + `"}}`,
		`{"id":"m2","timestamp":"` + tsEarlyS5 + `","type":"gemini","content":"Hi there!","model":"gemini-3.1-pro-preview","tokens":{"input":10,"output":5,"cached":0}}`,
	}, "\n")

	path := env.writeGeminiSession(
		t,
		filepath.Join(
			"tmp", hash, "chats",
			"session-001.jsonl",
		),
		content,
	)

	env.engine.SyncPaths([]string{path})

	assertSessionState(
		t, env.db, "gemini:"+sessionID,
		func(sess *db.Session) {
			assert.Equal(t, "gemini", sess.Agent, "agent = %q, want gemini", sess.Agent)
		},
	)
	assertSessionMessageCount(t, env.db, "gemini:"+sessionID, 2)
}

func TestSyncPathsGeminiProjectMetadataEventRefreshesProject(t *testing.T) {
	env := setupTestEnv(t)

	sessionID := "gem-project-refresh"
	projectsPath := filepath.Join(env.geminiDir, "projects.json")
	writeProject := func(name string) {
		t.Helper()
		require.NoError(t, os.WriteFile(
			projectsPath,
			fmt.Appendf(nil,
				`{"projects":{"/Users/alice/code/%s":"alias"}}`,
				name,
			),
			0o644,
		), "write projects")
	}
	writeProject("one")
	path := env.writeGeminiSession(
		t,
		filepath.Join(
			"tmp", "alias", "chats",
			"session-001.json",
		),
		testjsonl.GeminiSessionJSON(
			sessionID, "alias", tsEarly, tsEarlyS5,
			[]map[string]any{
				testjsonl.GeminiUserMsg(
					"m1", tsEarly, "Hello Gemini",
				),
				testjsonl.GeminiAssistantMsg(
					"m2", tsEarlyS5, "Hi there!", nil,
				),
			},
		),
	)

	env.engine.SyncPaths([]string{path})
	assertSessionState(
		t, env.db, "gemini:"+sessionID,
		func(sess *db.Session) {
			assert.Equal(t, "one", sess.Project)
		},
	)

	info, err := os.Stat(path)
	require.NoError(t, err, "stat gemini session")
	env.engine.InjectSkipCache(map[string]int64{
		path: info.ModTime().UnixNano(),
	})

	writeProject("two")
	env.engine.SyncPaths([]string{projectsPath})

	assertSessionState(
		t, env.db, "gemini:"+sessionID,
		func(sess *db.Session) {
			assert.Equal(t, "two", sess.Project)
		},
	)

	writeProject("three")
	env.engine.SyncPaths([]string{path, projectsPath})

	assertSessionState(
		t, env.db, "gemini:"+sessionID,
		func(sess *db.Session) {
			assert.Equal(t, "three", sess.Project)
		},
	)

	writeProject("four")
	env.engine.SyncPaths([]string{projectsPath, path})

	assertSessionState(
		t, env.db, "gemini:"+sessionID,
		func(sess *db.Session) {
			assert.Equal(t, "four", sess.Project)
		},
	)

	require.NoError(t, os.Remove(projectsPath), "remove projects")
	env.engine.SyncPaths([]string{projectsPath})

	assertSessionState(
		t, env.db, "gemini:"+sessionID,
		func(sess *db.Session) {
			assert.Equal(t, "alias", sess.Project)
		},
	)
}

// TestSyncAllGeminiProjectMetadataChangeReparsesProject guards that a scheduled
// SyncAll is not fooled by the pre-fingerprint fast skip when only Gemini's
// projects.json changed. The session transcript's own size and mtime are left
// untouched, so the removed AgentGemini fast skip (which compared just the
// session stat) would have kept the stale project; the composite fingerprint,
// which folds in projects.json, must drive a reparse on the periodic full sync.
func TestSyncAllGeminiProjectMetadataChangeReparsesProject(t *testing.T) {
	env := setupTestEnv(t)

	sessionID := "gem-syncall-refresh"
	projectsPath := filepath.Join(env.geminiDir, "projects.json")
	writeProject := func(name string) {
		t.Helper()
		require.NoError(t, os.WriteFile(
			projectsPath,
			fmt.Appendf(nil,
				`{"projects":{"/Users/alice/code/%s":"alias"}}`,
				name,
			),
			0o644,
		), "write projects")
	}
	writeProject("one")
	env.writeGeminiSession(
		t,
		filepath.Join("tmp", "alias", "chats", "session-001.json"),
		testjsonl.GeminiSessionJSON(
			sessionID, "alias", tsEarly, tsEarlyS5,
			[]map[string]any{
				testjsonl.GeminiUserMsg("m1", tsEarly, "Hello Gemini"),
				testjsonl.GeminiAssistantMsg("m2", tsEarlyS5, "Hi there!", nil),
			},
		),
	)

	env.engine.SyncAll(context.Background(), nil)
	assertSessionState(t, env.db, "gemini:"+sessionID, func(sess *db.Session) {
		assert.Equal(t, "one", sess.Project)
	})

	// Change only the project metadata, then advance its mtime so the composite
	// fingerprint moves forward. The session transcript is left untouched, so a
	// fast skip keyed on the transcript stat alone would keep the stale "one".
	writeProject("two")
	later := time.Now().Add(48 * time.Hour)
	require.NoError(t, os.Chtimes(projectsPath, later, later), "bump projects mtime")
	env.engine.SyncAll(context.Background(), nil)

	assertSessionState(t, env.db, "gemini:"+sessionID, func(sess *db.Session) {
		assert.Equal(t, "two", sess.Project)
	})
}

func TestSyncPathsCodexAcceptsFlatArchived(t *testing.T) {
	env := setupTestEnv(t)

	uuid := "d4e5f6a7-4567-8901-bcde-f12345678901"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			tsEarly, uuid,
			"/home/user/code/api", "user",
		).
		AddCodexMessage(tsEarlyS1, "user", "Add tests").
		String()

	path := env.writeSession(
		t, env.codexDir,
		"rollout-flat-"+uuid+".jsonl", content,
	)

	env.engine.SyncPaths([]string{path})

	sess, err := env.db.GetSession(
		context.Background(), "codex:"+uuid,
	)
	require.NoError(t, err, "GetSession")
	require.NotNil(t, sess, "expected flat archived Codex session to sync")
	assert.Equal(t, path, env.db.GetSessionFilePath("codex:"+uuid))
}

func TestSyncPathsCodexPrefersLivePathOverArchived(t *testing.T) {
	env := setupTestEnv(t)

	uuid := "e5f6a7b8-5678-9012-cdef-234567890123"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			tsEarly, uuid,
			"/home/user/code/api", "user",
		).
		AddCodexMessage(tsEarlyS1, "user", "Dedupe me").
		String()

	livePath := env.writeCodexSession(
		t,
		filepath.Join("2026", "05", "04"),
		"rollout-2026-05-04T02-10-04-"+uuid+".jsonl",
		content,
	)
	archivedPath := env.writeSession(
		t, env.codexDir,
		"rollout-2026-05-04T14-31-58-"+uuid+".jsonl",
		content,
	)

	env.engine.SyncAll(context.Background(), nil)

	sess, err := env.db.GetSession(
		context.Background(), "codex:"+uuid,
	)
	require.NoError(t, err, "GetSession")
	require.NotNil(t, sess, "expected Codex session to sync")
	assert.Equal(t, livePath, env.db.GetSessionFilePath("codex:"+uuid))
	assertSessionMessageCount(t, env.db, "codex:"+uuid, 1)
	_ = archivedPath
}

func TestSyncAllSinceCodexKeepsChangedArchivedDuplicate(t *testing.T) {
	env := setupTestEnv(t)

	uuid := "f6a7b8c9-6789-0123-def0-345678901234"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			tsEarly, uuid,
			"/home/user/code/api", "user",
		).
		AddCodexMessage(tsEarlyS1, "user", "Sync changed archive").
		String()

	livePath := env.writeCodexSession(
		t,
		filepath.Join("2026", "05", "04"),
		"rollout-2026-05-04T02-10-04-"+uuid+".jsonl",
		content,
	)
	archivedPath := env.writeSession(
		t, env.codexDir,
		"rollout-2026-05-04T14-31-58-"+uuid+".jsonl",
		content,
	)

	oldTime := time.Now().Add(-2 * time.Hour)
	newTime := time.Now().Add(-30 * time.Minute)
	cutoff := time.Now().Add(-1 * time.Hour)
	require.NoError(t, os.Chtimes(livePath, oldTime, oldTime), "chtimes live")
	require.NoError(t, os.Chtimes(archivedPath, newTime, newTime), "chtimes archived")

	stats := env.engine.SyncAllSince(context.Background(), cutoff, nil)
	require.Equal(t, 1, stats.Synced, "SyncAllSince synced = %d, want 1", stats.Synced)

	sess, err := env.db.GetSession(
		context.Background(), "codex:"+uuid,
	)
	require.NoError(t, err, "GetSession")
	require.NotNil(t, sess, "expected archived Codex session to sync")
	assert.Equal(t, archivedPath, env.db.GetSessionFilePath("codex:"+uuid))
}

func TestSyncAllSinceCodexRefreshesSessionNameFromIndex(t *testing.T) {
	root := t.TempDir()
	codexDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	env := setupTestEnv(t, WithCodexDirs([]string{codexDir}))

	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e1"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			tsEarly, uuid,
			"/home/user/code/api", "user",
		).
		AddCodexMessage(tsEarlyS1, "user", "Rename me").
		String()
	path := env.writeCodexSession(
		t,
		filepath.Join("2026", "06", "11"),
		"rollout-2026-06-11T12-44-06-"+uuid+".jsonl",
		content,
	)

	indexPath := filepath.Join(root, "session_index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Original title","updated_at":"2026-06-11T17:34:20Z"}`+"\n",
		uuid,
	), 0o644))
	initialTime := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(path, initialTime, initialTime), "chtimes initial session")
	require.NoError(t, os.Chtimes(indexPath, initialTime, initialTime), "chtimes initial index")

	env.engine.SyncAll(context.Background(), nil)
	sess, err := env.db.GetSessionFull(context.Background(), "codex:"+uuid)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, sess, "expected Codex session to sync")
	if assert.NotNil(t, sess.SessionName, "expected session_name to be imported") {
		assert.Equal(t, "Original title", *sess.SessionName)
	}

	cutoff := time.Now().Add(-1 * time.Hour)
	newIndexTime := time.Now().Add(-30 * time.Minute)
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Renamed title","updated_at":"2026-06-11T18:00:00Z"}`+"\n",
		uuid,
	), 0o644))
	require.NoError(t, os.Chtimes(indexPath, newIndexTime, newIndexTime), "chtimes index")

	stats := env.engine.SyncAllSince(context.Background(), cutoff, nil)
	require.Equal(t, 1, stats.Synced, "SyncAllSince synced = %d, want 1", stats.Synced)

	sess, err = env.db.GetSessionFull(context.Background(), "codex:"+uuid)
	require.NoError(t, err, "GetSessionFull after rename")
	require.NotNil(t, sess, "expected renamed Codex session to remain")
	if assert.NotNil(t, sess.SessionName, "expected renamed session_name") {
		assert.Equal(t, "Renamed title", *sess.SessionName)
	}
}

// TestSyncAllSinceCodexIndexRenameBelowStoredMtimeRefreshesName covers the
// quick-sync sibling of the incremental masking race: a title-only index rename
// whose mtime lands at/after the cutoff but at or below the stored (index-folded)
// file_mtime must still refresh the session_name. codexIndexNeedsRefreshSince
// must compare the title directly instead of gating on indexMtime > storedMtime.
func TestSyncAllSinceCodexIndexRenameBelowStoredMtimeRefreshesName(t *testing.T) {
	root := t.TempDir()
	codexDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	env := setupTestEnv(t, WithCodexDirs([]string{codexDir}))

	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e1"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Rename me").
		String()
	path := env.writeCodexSession(
		t,
		filepath.Join("2026", "06", "11"),
		"rollout-2026-06-11T12-44-06-"+uuid+".jsonl",
		content,
	)

	indexPath := filepath.Join(root, "session_index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Original title","updated_at":"2026-06-11T17:34:20Z"}`+"\n",
		uuid,
	), 0o644))

	// Transcript is well before the cutoff; the index is newer, so the initial
	// parse stores the high index mtime as the effective file_mtime.
	transcriptTime := time.Now().Add(-3 * time.Hour)
	highIndexTime := time.Now().Add(-30 * time.Minute)
	require.NoError(t, os.Chtimes(path, transcriptTime, transcriptTime), "chtimes initial session")
	require.NoError(t, os.Chtimes(indexPath, highIndexTime, highIndexTime), "chtimes initial index")

	env.engine.SyncAll(context.Background(), nil)
	sess, err := env.db.GetSessionFull(context.Background(), "codex:"+uuid)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, sess, "expected Codex session to sync")
	require.NotNil(t, sess.SessionName, "expected session_name to be imported")
	require.Equal(t, "Original title", *sess.SessionName)
	require.NotNil(t, sess.FileMtime, "expected stored file_mtime")
	require.Equal(t, highIndexTime.UnixNano(), *sess.FileMtime,
		"expected stored file_mtime to fold in the higher index mtime")

	// Rename the index with an mtime that is after the cutoff but BELOW the
	// stored file_mtime. The old indexMtime > storedMtime gate would filter this
	// out of the quick sync, stranding the stale title.
	cutoff := time.Now().Add(-1 * time.Hour)
	lowIndexTime := time.Now().Add(-45 * time.Minute)
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Renamed title","updated_at":"2026-06-11T18:00:00Z"}`+"\n",
		uuid,
	), 0o644))
	require.NoError(t, os.Chtimes(indexPath, lowIndexTime, lowIndexTime), "chtimes renamed index")

	stats := env.engine.SyncAllSince(context.Background(), cutoff, nil)
	require.Equal(t, 1, stats.Synced, "SyncAllSince synced = %d, want 1", stats.Synced)

	sess, err = env.db.GetSessionFull(context.Background(), "codex:"+uuid)
	require.NoError(t, err, "GetSessionFull after rename")
	require.NotNil(t, sess, "expected renamed Codex session to remain")
	if assert.NotNil(t, sess.SessionName, "expected renamed session_name") {
		assert.Equal(t, "Renamed title", *sess.SessionName)
	}
}

func TestSyncAllSinceCodexIndexRefreshOnlySyncsRenamedSession(t *testing.T) {
	root := t.TempDir()
	codexDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	env := setupTestEnv(t, WithCodexDirs([]string{codexDir}))

	renamedUUID := "019eb791-cf7d-75c1-8439-9ed74c1229e1"
	unchangedUUID := "019eb791-cf7d-75c1-8439-9ed74c1229e2"
	renamedContent := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			tsEarly, renamedUUID,
			"/home/user/code/api", "user",
		).
		AddCodexMessage(tsEarlyS1, "user", "Rename me").
		String()
	unchangedContent := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			tsEarly, unchangedUUID,
			"/home/user/code/api", "user",
		).
		AddCodexMessage(tsEarlyS1, "user", "Leave me").
		String()
	renamedPath := env.writeCodexSession(
		t,
		filepath.Join("2026", "06", "11"),
		"rollout-2026-06-11T12-44-06-"+renamedUUID+".jsonl",
		renamedContent,
	)
	unchangedPath := env.writeCodexSession(
		t,
		filepath.Join("2026", "06", "11"),
		"rollout-2026-06-11T12-45-06-"+unchangedUUID+".jsonl",
		unchangedContent,
	)

	indexPath := filepath.Join(root, "session_index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Original renamed title","updated_at":"2026-06-11T17:34:20Z"}`+"\n"+
			`{"id":"%s","thread_name":"Unchanged title","updated_at":"2026-06-11T17:35:20Z"}`+"\n",
		renamedUUID, unchangedUUID,
	), 0o644))
	initialTime := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(renamedPath, initialTime, initialTime), "chtimes initial renamed")
	require.NoError(t, os.Chtimes(unchangedPath, initialTime, initialTime), "chtimes initial unchanged")
	require.NoError(t, os.Chtimes(indexPath, initialTime, initialTime), "chtimes initial index")

	env.engine.SyncAll(context.Background(), nil)
	unchangedBefore, err := env.db.GetSessionFull(context.Background(), "codex:"+unchangedUUID)
	require.NoError(t, err, "GetSessionFull unchanged before rename")
	require.NotNil(t, unchangedBefore, "expected unchanged Codex session to sync")
	require.NotNil(t, unchangedBefore.SessionName, "expected unchanged session_name")
	assert.Equal(t, "Unchanged title", *unchangedBefore.SessionName)
	require.NotNil(t, unchangedBefore.FileMtime, "expected unchanged file_mtime")

	cutoff := time.Now().Add(-1 * time.Hour)
	newIndexTime := time.Now().Add(-30 * time.Minute)
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Renamed title","updated_at":"2026-06-11T18:00:00Z"}`+"\n"+
			`{"id":"%s","thread_name":"Unchanged title","updated_at":"2026-06-11T17:35:20Z"}`+"\n",
		renamedUUID, unchangedUUID,
	), 0o644))
	require.NoError(t, os.Chtimes(indexPath, newIndexTime, newIndexTime), "chtimes index")

	stats := env.engine.SyncAllSince(context.Background(), cutoff, nil)
	require.Equal(t, 1, stats.Synced, "SyncAllSince synced = %d, want 1", stats.Synced)

	renamed, err := env.db.GetSessionFull(context.Background(), "codex:"+renamedUUID)
	require.NoError(t, err, "GetSessionFull renamed after index update")
	require.NotNil(t, renamed, "expected renamed Codex session to remain")
	if assert.NotNil(t, renamed.SessionName, "expected renamed session_name") {
		assert.Equal(t, "Renamed title", *renamed.SessionName)
	}

	unchangedAfter, err := env.db.GetSessionFull(context.Background(), "codex:"+unchangedUUID)
	require.NoError(t, err, "GetSessionFull unchanged after index update")
	require.NotNil(t, unchangedAfter, "expected unchanged Codex session to remain")
	if assert.NotNil(t, unchangedAfter.SessionName, "expected unchanged session_name") {
		assert.Equal(t, "Unchanged title", *unchangedAfter.SessionName)
	}
	assert.Equal(t, *unchangedBefore.FileMtime, *unchangedAfter.FileMtime,
		"unchanged session should not be reparsed just because the global index mtime advanced")

	unchangedIndexTime := time.Now().Add(-15 * time.Minute)
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Renamed title","updated_at":"2026-06-11T18:00:00Z"}`+"\n"+
			`{"id":"%s","thread_name":"Unchanged title","updated_at":"2026-06-11T17:35:20Z"}`+"\n",
		renamedUUID, unchangedUUID,
	), 0o644))
	require.NoError(t, os.Chtimes(indexPath, unchangedIndexTime, unchangedIndexTime), "chtimes unchanged index")

	stats = env.engine.SyncAll(context.Background(), nil)
	require.Equal(t, 0, stats.Synced,
		"SyncAll synced = %d, want 0 when only the global index mtime changed", stats.Synced)
}

func TestSyncPathsCodexIndexEventRefreshesRenamedSession(t *testing.T) {
	root := t.TempDir()
	codexDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	env := setupTestEnv(t, WithCodexDirs([]string{codexDir}))

	renamedUUID := "019eb791-cf7d-75c1-8439-9ed74c1229e1"
	unchangedUUID := "019eb791-cf7d-75c1-8439-9ed74c1229e2"
	renamedPath := env.writeCodexSession(
		t,
		filepath.Join("2026", "06", "11"),
		"rollout-2026-06-11T12-44-06-"+renamedUUID+".jsonl",
		testjsonl.NewSessionBuilder().
			AddCodexMeta(tsEarly, renamedUUID, "/home/user/code/api", "user").
			AddCodexMessage(tsEarlyS1, "user", "Rename me").
			String(),
	)
	unchangedPath := env.writeCodexSession(
		t,
		filepath.Join("2026", "06", "11"),
		"rollout-2026-06-11T12-45-06-"+unchangedUUID+".jsonl",
		testjsonl.NewSessionBuilder().
			AddCodexMeta(tsEarly, unchangedUUID, "/home/user/code/api", "user").
			AddCodexMessage(tsEarlyS1, "user", "Leave me").
			String(),
	)

	indexPath := filepath.Join(root, "session_index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Original title","updated_at":"2026-06-11T17:34:20Z"}`+"\n"+
			`{"id":"%s","thread_name":"Unchanged title","updated_at":"2026-06-11T17:35:20Z"}`+"\n",
		renamedUUID, unchangedUUID,
	), 0o644))
	initialTime := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(renamedPath, initialTime, initialTime), "chtimes renamed")
	require.NoError(t, os.Chtimes(unchangedPath, initialTime, initialTime), "chtimes unchanged")
	require.NoError(t, os.Chtimes(indexPath, initialTime, initialTime), "chtimes index")

	env.engine.SyncAll(context.Background(), nil)
	unchangedBefore, err := env.db.GetSessionFull(context.Background(), "codex:"+unchangedUUID)
	require.NoError(t, err, "GetSessionFull unchanged before rename")
	require.NotNil(t, unchangedBefore, "expected unchanged Codex session to sync")
	require.NotNil(t, unchangedBefore.FileMtime, "expected unchanged file_mtime")

	newIndexTime := time.Now().Add(-30 * time.Minute)
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Renamed title","updated_at":"2026-06-11T18:00:00Z"}`+"\n"+
			`{"id":"%s","thread_name":"Unchanged title","updated_at":"2026-06-11T17:35:20Z"}`+"\n",
		renamedUUID, unchangedUUID,
	), 0o644))
	require.NoError(t, os.Chtimes(indexPath, newIndexTime, newIndexTime), "chtimes index")

	// The live watcher only delivers the index path; SyncPaths must translate
	// it into a refresh of the renamed session.
	env.engine.SyncPaths([]string{indexPath})

	renamed, err := env.db.GetSessionFull(context.Background(), "codex:"+renamedUUID)
	require.NoError(t, err, "GetSessionFull renamed after index event")
	require.NotNil(t, renamed, "expected renamed Codex session to remain")
	if assert.NotNil(t, renamed.SessionName, "expected renamed session_name") {
		assert.Equal(t, "Renamed title", *renamed.SessionName)
	}

	unchangedAfter, err := env.db.GetSessionFull(context.Background(), "codex:"+unchangedUUID)
	require.NoError(t, err, "GetSessionFull unchanged after index event")
	require.NotNil(t, unchangedAfter, "expected unchanged Codex session to remain")
	require.NotNil(t, unchangedAfter.FileMtime, "expected unchanged file_mtime after event")
	assert.Equal(t, *unchangedBefore.FileMtime, *unchangedAfter.FileMtime,
		"unchanged session must not be reparsed from an index-only event")
}

func TestSyncAllSinceCodexIndexRefreshDoesNotShadowChangedArchivedDuplicate(t *testing.T) {
	root := t.TempDir()
	codexDir := filepath.Join(root, "sessions")
	archivedDir := filepath.Join(root, "archived_sessions")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	require.NoError(t, os.MkdirAll(archivedDir, 0o755))
	env := setupTestEnv(t, WithCodexDirs([]string{codexDir, archivedDir}))

	uuid := "f7a8b9ca-7890-1234-ef01-456789012345"
	liveContent := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			tsEarly, uuid,
			"/home/user/code/api", "user",
		).
		AddCodexMessage(tsEarlyS1, "user", "Live copy").
		String()
	archivedContent := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			tsEarly, uuid,
			"/home/user/code/api", "user",
		).
		AddCodexMessage(tsEarlyS1, "user", "Archived copy").
		AddCodexMessage(tsEarlyS5, "assistant", "Updated archive").
		String()

	livePath := env.writeCodexSession(
		t,
		filepath.Join("2026", "05", "04"),
		"rollout-2026-05-04T02-10-04-"+uuid+".jsonl",
		liveContent,
	)
	archivedPath := env.writeSession(
		t,
		archivedDir,
		"rollout-2026-05-04T14-31-58-"+uuid+".jsonl",
		liveContent,
	)

	indexPath := filepath.Join(root, "session_index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Original title","updated_at":"2026-05-04T15:00:00Z"}`+"\n",
		uuid,
	), 0o644))
	initialTime := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(livePath, initialTime, initialTime), "chtimes initial live")
	require.NoError(t, os.Chtimes(archivedPath, initialTime, initialTime), "chtimes initial archived")
	require.NoError(t, os.Chtimes(indexPath, initialTime, initialTime), "chtimes initial index")

	env.engine.SyncAll(context.Background(), nil)
	assert.Equal(t, livePath, env.db.GetSessionFilePath("codex:"+uuid))

	newTime := time.Now().Add(-30 * time.Minute)
	cutoff := time.Now().Add(-1 * time.Hour)
	require.NoError(t, os.WriteFile(archivedPath, []byte(archivedContent), 0o644))
	require.NoError(t, os.Chtimes(archivedPath, newTime, newTime), "chtimes archived")
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Renamed title","updated_at":"2026-05-04T16:00:00Z"}`+"\n",
		uuid,
	), 0o644))
	require.NoError(t, os.Chtimes(indexPath, newTime, newTime), "chtimes index")

	stats := env.engine.SyncAllSince(context.Background(), cutoff, nil)
	require.Equal(t, 1, stats.Synced, "SyncAllSince synced = %d, want 1", stats.Synced)

	sess, err := env.db.GetSessionFull(context.Background(), "codex:"+uuid)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, sess, "expected Codex session to sync")
	assert.Equal(t, archivedPath, env.db.GetSessionFilePath("codex:"+uuid))
	if assert.NotNil(t, sess.SessionName, "expected renamed session_name") {
		assert.Equal(t, "Renamed title", *sess.SessionName)
	}
	assertSessionMessageCount(t, env.db, "codex:"+uuid, 2)
}

func TestSyncPathsCodexIndexEventRefreshesStoredDuplicate(t *testing.T) {
	root := t.TempDir()
	codexDir := filepath.Join(root, "sessions")
	archivedDir := filepath.Join(root, "archived_sessions")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	require.NoError(t, os.MkdirAll(archivedDir, 0o755))
	env := setupTestEnv(t, WithCodexDirs([]string{codexDir, archivedDir}))

	uuid := "f7a8b9ca-7890-1234-ef01-456789012345"
	archivedContent := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Archived copy").
		AddCodexMessage(tsEarlyS5, "assistant", "Archived reply").
		String()
	staleLiveContent := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Stale live copy").
		String()

	// Only the archived copy exists at first sync, so the DB tracks it.
	archivedPath := env.writeSession(
		t, archivedDir,
		"rollout-2026-05-04T14-31-58-"+uuid+".jsonl",
		archivedContent,
	)
	indexPath := filepath.Join(root, "session_index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Original title","updated_at":"2026-05-04T15:00:00Z"}`+"\n",
		uuid,
	), 0o644))
	initialTime := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(archivedPath, initialTime, initialTime), "chtimes archived")
	require.NoError(t, os.Chtimes(indexPath, initialTime, initialTime), "chtimes index")

	env.engine.SyncAll(context.Background(), nil)
	require.Equal(t, archivedPath, env.db.GetSessionFilePath("codex:"+uuid),
		"DB must track the archived copy before the rename")

	// A stale live duplicate now exists, and the index records a rename.
	livePath := env.writeCodexSession(
		t,
		filepath.Join("2026", "05", "04"),
		"rollout-2026-05-04T02-10-04-"+uuid+".jsonl",
		staleLiveContent,
	)
	require.NoError(t, os.Chtimes(livePath, initialTime, initialTime), "chtimes live")
	newIndexTime := time.Now().Add(-30 * time.Minute)
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Renamed title","updated_at":"2026-05-04T16:00:00Z"}`+"\n",
		uuid,
	), 0o644))
	require.NoError(t, os.Chtimes(indexPath, newIndexTime, newIndexTime), "chtimes index rename")

	env.engine.SyncPaths([]string{indexPath})

	sess, err := env.db.GetSessionFull(context.Background(), "codex:"+uuid)
	require.NoError(t, err, "GetSessionFull after index event")
	require.NotNil(t, sess, "expected Codex session to remain")
	assert.Equal(t, archivedPath, env.db.GetSessionFilePath("codex:"+uuid),
		"index rename must refresh the stored archived copy, not the stale live duplicate")
	if assert.NotNil(t, sess.SessionName, "expected renamed session_name") {
		assert.Equal(t, "Renamed title", *sess.SessionName)
	}
	assertSessionMessageCount(t, env.db, "codex:"+uuid, 2)
}

func TestSyncPathsCodexArchivedDuplicateEventPinsChangedFile(t *testing.T) {
	root := t.TempDir()
	codexDir := filepath.Join(root, "sessions")
	archivedDir := filepath.Join(root, "archived_sessions")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	require.NoError(t, os.MkdirAll(archivedDir, 0o755))
	env := setupTestEnv(t, WithCodexDirs([]string{codexDir, archivedDir}))

	uuid := "f7a8b9ca-7890-1234-ef01-456789012346"
	staleLiveContent := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Stale live copy").
		String()
	archivedContent := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Archived copy").
		String()
	updatedArchivedContent := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Archived copy").
		AddCodexMessage(tsEarlyS5, "assistant", "Updated archived reply").
		String()

	livePath := env.writeCodexSession(
		t,
		filepath.Join("2026", "05", "04"),
		"rollout-2026-05-04T02-10-04-"+uuid+".jsonl",
		staleLiveContent,
	)
	archivedPath := env.writeSession(
		t, archivedDir,
		"rollout-2026-05-04T14-31-58-"+uuid+".jsonl",
		archivedContent,
	)
	initialTime := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(livePath, initialTime, initialTime), "chtimes live")
	require.NoError(t, os.Chtimes(archivedPath, initialTime, initialTime), "chtimes archived")

	env.engine.SyncAll(context.Background(), nil)
	assert.Equal(t, livePath, env.db.GetSessionFilePath("codex:"+uuid))

	newTime := time.Now().Add(-30 * time.Minute)
	require.NoError(t, os.WriteFile(archivedPath, []byte(updatedArchivedContent), 0o644))
	require.NoError(t, os.Chtimes(archivedPath, newTime, newTime), "chtimes archived update")

	env.engine.SyncPaths([]string{archivedPath})

	assert.Equal(t, archivedPath, env.db.GetSessionFilePath("codex:"+uuid),
		"archived transcript event must parse the changed file, not the stale live duplicate")
	assertSessionMessageCount(t, env.db, "codex:"+uuid, 2)
}

func TestSyncSingleSessionCodexPreservesStoredArchivedDuplicate(t *testing.T) {
	root := t.TempDir()
	codexDir := filepath.Join(root, "sessions")
	archivedDir := filepath.Join(root, "archived_sessions")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	require.NoError(t, os.MkdirAll(archivedDir, 0o755))
	env := setupSingleAgentTestEnvWithDirs(
		t, parser.AgentCodex, []string{codexDir, archivedDir},
	)

	uuid := "f7a8b9ca-7890-1234-ef01-456789012347"
	archivedContent := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Archived copy").
		AddCodexMessage(tsEarlyS5, "assistant", "Archived reply").
		String()
	staleLiveContent := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Stale live copy").
		String()

	archivedPath := env.writeSession(
		t, archivedDir,
		"rollout-2026-05-04T14-31-58-"+uuid+".jsonl",
		archivedContent,
	)
	initialTime := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(archivedPath, initialTime, initialTime), "chtimes archived")

	env.engine.SyncAll(context.Background(), nil)
	require.Equal(t, archivedPath, env.db.GetSessionFilePath("codex:"+uuid),
		"DB must track the archived copy before a stale live duplicate appears")

	livePath := env.writeCodexSession(
		t,
		filepath.Join("2026", "05", "04"),
		"rollout-2026-05-04T02-10-04-"+uuid+".jsonl",
		staleLiveContent,
	)
	require.NoError(t, os.Chtimes(livePath, initialTime, initialTime), "chtimes live")

	require.NoError(t, env.engine.SyncSingleSession("codex:"+uuid))

	assert.Equal(t, archivedPath, env.db.GetSessionFilePath("codex:"+uuid),
		"single-session resync must preserve the stored archived source")
	assertSessionMessageCount(t, env.db, "codex:"+uuid, 2)
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
	assert.Nil(t, sess, "Gemini file outside tmp/<hash>/chats "+"should be ignored")
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
			assert.Equal(t, "amp", sess.Agent, "agent = %q, want amp", sess.Agent)
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
	assert.Nil(t, sess, "Amp files outside root-level valid T-<id>.json should be ignored")
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
	assert.Equal(t, 1, stats.Synced, "LastSyncStats.Synced = %d, want 1", stats.Synced)
	lastSync := env.engine.LastSync()
	assert.False(t, lastSync.IsZero(), "LastSync should be set after SyncPaths")
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
			require.NotNil(t, sess.ParentSessionID,
				"parent_session_id = %v, want %q", sess.ParentSessionID, "parent-uuid")
			assert.Equal(t, "parent-uuid", *sess.ParentSessionID,
				"parent_session_id = %v, want %q", sess.ParentSessionID, "parent-uuid")
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
			assert.Nil(t, sess.ParentSessionID, "parent_session_id = %v, want nil", sess.ParentSessionID)
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
			assert.Nil(t, sess.ParentSessionID, "parent parent_session_id = %v, want nil", sess.ParentSessionID)
		},
	)

	// Verify subagent has parent_session_id set
	assertSessionState(
		t, env.db, "agent-worker1",
		func(sess *db.Session) {
			require.NotNil(t, sess.ParentSessionID,
				"subagent parent_session_id = %v, want %q", sess.ParentSessionID, "parent-uuid")
			assert.Equal(t, "parent-uuid", *sess.ParentSessionID,
				"subagent parent_session_id = %v, want %q", sess.ParentSessionID, "parent-uuid")
			assert.Equal(t, "claude", sess.Agent, "agent = %q, want claude", sess.Agent)
		},
	)
	assertSessionMessageCount(t, env.db, "agent-worker1", 2)

	// Verify FindSourceFile works for subagent
	src := env.engine.FindSourceFile("agent-worker1")
	assert.NotEmpty(t, src, "FindSourceFile returned empty for subagent")
}

func TestSyncClaudeToolResultAgentIDLinksSubagentToolCall(t *testing.T) {
	env := setupTestEnv(t)

	parentContent := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"Build the feature"},"cwd":"/tmp"}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"u2","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_agent_result","name":"Agent","input":{"description":"inspect schema","subagent_type":"Explore","prompt":"inspect the schema"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u3","parentUuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_agent_result","content":"done"}]},"toolUseResult":{"status":"completed","agentId":"abc123def4567890"}}`,
	}, "\n")

	env.writeClaudeSession(
		t, "test-proj", "parent-agentid.jsonl", parentContent,
	)

	subContent := testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID(
			tsEarly, "Do subtask", "parent-agentid",
		).
		AddClaudeAssistant(tsEarlyS5, "Subtask done.").
		String()

	env.writeSession(
		t, env.claudeDir,
		filepath.Join(
			"test-proj", "parent-agentid",
			"subagents", "agent-abc123def4567890.jsonl",
		),
		subContent,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 2, Synced: 2, Skipped: 0})

	var got string
	err := env.db.Reader().QueryRow(`
		SELECT subagent_session_id
		FROM tool_calls
		WHERE session_id = ? AND tool_use_id = ?`,
		"parent-agentid", "toolu_agent_result",
	).Scan(&got)
	require.NoError(t, err, "query linked subagent tool call")
	assert.Equal(t, "agent-abc123def4567890", got, "subagent_session_id = %q, want %q", got, "agent-abc123def4567890")
}

func TestSyncClaudeSameMessageIDAgentChunksLinkAllSubagents(t *testing.T) {
	env := setupTestEnv(t)

	parentContent := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"summarize with subagents"},"cwd":"/tmp"}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"u1","message":{"id":"msg_same","content":[{"type":"text","text":"Launching agents."}],"usage":{"input_tokens":1,"output_tokens":1},"stop_reason":"tool_use"}}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:02Z","uuid":"a2","parentUuid":"a1","message":{"id":"msg_same","content":[{"type":"tool_use","id":"toolu_first","name":"Agent","input":{"description":"first","subagent_type":"Explore","prompt":"first"}}],"usage":{"input_tokens":1,"output_tokens":2},"stop_reason":"tool_use"}}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:03Z","uuid":"a3","parentUuid":"a2","message":{"id":"msg_same","content":[{"type":"tool_use","id":"toolu_second","name":"Agent","input":{"description":"second","subagent_type":"Explore","prompt":"second"}}],"usage":{"input_tokens":1,"output_tokens":3},"stop_reason":"tool_use"}}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:04Z","uuid":"a4","parentUuid":"a3","message":{"id":"msg_same","content":[{"type":"tool_use","id":"toolu_third","name":"Agent","input":{"description":"third","subagent_type":"Explore","prompt":"third"}}],"usage":{"input_tokens":1,"output_tokens":4},"stop_reason":"tool_use"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"r1","parentUuid":"a2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_first","content":"done first"}]},"toolUseResult":{"status":"completed","agentId":"childfirst"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:06Z","uuid":"r2","parentUuid":"a3","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_second","content":"done second"}]},"toolUseResult":{"status":"completed","agentId":"childsecond"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:07Z","uuid":"r3","parentUuid":"a4","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_third","content":"done third"}]},"toolUseResult":{"status":"completed","agentId":"childthird"}}`,
	}, "\n")

	env.writeClaudeSession(
		t, "test-proj", "parent-same-message.jsonl", parentContent,
	)

	for _, child := range []string{"childfirst", "childsecond", "childthird"} {
		subContent := testjsonl.NewSessionBuilder().
			AddClaudeUserWithSessionID(
				tsEarly, "Do "+child, "parent-same-message",
			).
			AddClaudeAssistant(tsEarlyS5, child+" done.").
			String()
		env.writeSession(
			t, env.claudeDir,
			filepath.Join(
				"test-proj", "parent-same-message",
				"subagents", "agent-"+child+".jsonl",
			),
			subContent,
		)
	}

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 4, Synced: 4, Skipped: 0})

	rows, err := env.db.Reader().Query(`
		SELECT tool_use_id, subagent_session_id
		FROM tool_calls
		WHERE session_id = ?
		ORDER BY tool_use_id`,
		"parent-same-message",
	)
	require.NoError(t, err, "query linked subagent tool calls")
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var toolUseID, subagentSessionID string
		require.NoError(t, rows.Scan(&toolUseID, &subagentSessionID), "scan linked subagent tool call")
		got[toolUseID] = subagentSessionID
	}
	require.NoError(t, rows.Err(), "iterate linked subagent tool calls")

	want := map[string]string{
		"toolu_first":  "agent-childfirst",
		"toolu_second": "agent-childsecond",
		"toolu_third":  "agent-childthird",
	}
	require.Equal(t, len(want), len(got), "linked tool calls = %v, want %v", got, want)
	for toolUseID, wantSessionID := range want {
		assert.Equal(t, wantSessionID, got[toolUseID], "%s subagent_session_id = %q, want %q", toolUseID, got[toolUseID], wantSessionID)
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
			assert.Equal(t, "claude", sess.Agent, "agent = %q, want claude", sess.Agent)
		},
	)
}

func TestSyncPathsClaudeSubagentUsesCompanionDirectoryParent(
	t *testing.T,
) {
	env := setupTestEnv(t)

	parentContent := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "Hello").
		AddClaudeAssistant(tsZeroS5, "Hi!").
		String()

	env.writeClaudeSession(
		t, "test-proj", "parent-sess.jsonl", parentContent,
	)

	subagentContent := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "Do subtask").
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

	env.engine.SyncPaths([]string{subPath})

	assertSessionState(
		t, env.db, "agent-sub1",
		func(sess *db.Session) {
			require.NotNil(t, sess.ParentSessionID,
				"subagent parent_session_id = %v, want %q", sess.ParentSessionID, "parent-sess")
			assert.Equal(t, "parent-sess", *sess.ParentSessionID,
				"subagent parent_session_id = %v, want %q", sess.ParentSessionID, "parent-sess")
			assert.Equal(t, "subagent", sess.RelationshipType,
				"relationship_type = %q, want subagent", sess.RelationshipType)
		},
	)
}

func TestSyncPathsClaudeNestedWorkflowSubagent(t *testing.T) {
	env := setupTestEnv(t)

	subagentContent := testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID(
			tsZero, "Deep research", "parent-sess",
		).
		AddClaudeAssistant(tsZeroS5, "Done.").
		String()

	subPath := env.writeSession(
		t, env.claudeDir,
		filepath.Join(
			"test-proj", "parent-sess",
			"subagents", "workflows", "wf-123",
			"agent-deep.jsonl",
		),
		subagentContent,
	)

	env.engine.SyncPaths([]string{subPath})

	assertSessionState(
		t, env.db, "agent-deep",
		func(sess *db.Session) {
			assert.Equal(t, "claude", sess.Agent, "agent = %q, want claude", sess.Agent)
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
	assert.Nil(t, sess, "non-agent file in subagents dir "+"should be rejected")
}

func TestSyncPathsClaudeRejectsNested(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

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
	assert.Nil(t, sess, "nested Claude path should be rejected "+"(only <project>/<session>.jsonl allowed)")
}

// TestSyncEngineOpenCodeBulkSync verifies that SyncAll
// discovers OpenCode sessions and fully replaces messages
// when content changes in place (same ordinals, different
// text/tool data).
func TestSyncEngineOpenCodeBulkSync(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)

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
	env.engine.SyncAll(context.Background(), nil)

	agentviewID := "opencode:" + sessionID
	assertSessionState(t, env.db, agentviewID,
		func(sess *db.Session) {
			assert.Equal(t, "opencode", sess.Agent, "agent = %q, want opencode", sess.Agent)
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
	env.engine.SyncAll(context.Background(), nil)

	assertMessageContent(
		t, env.db, agentviewID,
		"updated question", "updated answer",
	)

	// Third SyncAll with no changes should be a no-op
	// (time_updated unchanged, so session is skipped).
	env.engine.SyncAll(context.Background(), nil)

	assertMessageContent(
		t, env.db, agentviewID,
		"updated question", "updated answer",
	)
}

func TestSyncEngineOpenCodeReviewWithGeneratedTitleIsAutomated(
	t *testing.T,
) {
	env := setupTestEnv(t)

	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj-1", "/home/user/code/myapp")

	sessionID := "oc-review-title"
	timeCreated := int64(1704067200000)
	timeUpdated := int64(1704067205000)
	oc.mustExec(t, "insert titled session",
		`INSERT INTO session
			(id, project_id, title, time_created, time_updated)
		 VALUES (?, ?, ?, ?, ?)`,
		sessionID, "proj-1", "Review generated title",
		timeCreated, timeUpdated,
	)
	oc.addMessage(t, "msg-u1", sessionID, "user", timeCreated)
	oc.addMessage(t, "msg-a1", sessionID, "assistant", timeCreated+1)
	oc.addTextPart(
		t, "part-u1", sessionID, "msg-u1",
		"You are a code reviewer. Review the code changes shown below.",
		timeCreated,
	)
	oc.addTextPart(
		t, "part-a1", sessionID, "msg-a1",
		"Review complete.", timeCreated+1,
	)

	env.engine.SyncAll(context.Background(), nil)

	agentviewID := "opencode:" + sessionID
	assertSessionState(t, env.db, agentviewID,
		func(sess *db.Session) {
			assert.True(t, sess.IsAutomated,
				"OpenCode review sessions with generated titles must still be classified as automated")
		},
	)

	page, err := env.db.ListSessions(
		context.Background(),
		db.SessionFilter{ExcludeAutomated: true, Limit: 10},
	)
	require.NoError(t, err, "ListSessions exclude automated")
	assert.Empty(t, page.Sessions,
		"automated OpenCode review should be excluded")
}

func TestSyncEngineOpenCodeStorageBulkSync(t *testing.T) {
	env := setupTestEnv(t)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionPath := oc.addSession(
		t, "global", "oc-storage-1",
		"/home/user/code/myapp", "Storage Sync",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, "oc-storage-1", "msg-u1", "user",
		1704067200000, nil,
	)
	oc.addTextPart(
		t, "oc-storage-1", "msg-u1", "part-u1",
		"hello from storage", 1704067200000,
	)
	oc.addMessage(
		t, "oc-storage-1", "msg-a1", "assistant",
		1704067201000, map[string]any{
			"modelID": "gpt-5.2-codex",
		},
	)
	oc.addTextPart(
		t, "oc-storage-1", "msg-a1", "part-a1",
		"reply from storage", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	assertSessionState(t, env.db, "opencode:oc-storage-1",
		func(sess *db.Session) {
			assert.Equal(t, "opencode", sess.Agent, "agent = %q, want opencode", sess.Agent)
		},
	)
	assert.Equal(t, sessionPath, env.engine.FindSourceFile("opencode:oc-storage-1"))
	assertMessageContent(
		t, env.db, "opencode:oc-storage-1",
		"hello from storage", "reply from storage",
	)
}

func TestSyncSingleSessionOpenCodeSQLiteFallback(t *testing.T) {
	env := setupTestEnv(t)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj-1", "/home/user/code/myapp")

	sessionID := "oc-sqlite-sync-single"
	timeCreated := int64(1704067200000)
	timeUpdated := int64(1704067205000)

	oc.addSession(
		t, sessionID, "proj-1",
		timeCreated, timeUpdated,
	)
	oc.addMessage(
		t, "msg-u1", sessionID, "user", timeCreated,
	)
	oc.addMessage(
		t, "msg-a1", sessionID, "assistant", timeCreated+1,
	)
	oc.addTextPart(
		t, "part-u1", sessionID, "msg-u1",
		"original sqlite question", timeCreated,
	)
	oc.addTextPart(
		t, "part-a1", sessionID, "msg-a1",
		"original sqlite answer", timeCreated+1,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	oc.replaceTextContent(
		t, sessionID,
		"updated sqlite question",
		"updated sqlite answer",
		timeCreated,
	)
	oc.updateSessionTime(t, sessionID, timeUpdated+1000)

	err := env.engine.SyncSingleSession(
		"opencode:" + sessionID,
	)
	require.NoError(t, err, "SyncSingleSession")

	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"updated sqlite question",
		"updated sqlite answer",
	)
}

func TestSyncSingleSessionOpenCodeSQLiteFallbackPreservesStorageArchive(
	t *testing.T,
) {
	env := setupTestEnv(t)
	storage := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionID := "oc-sqlite-single-preserve"
	storage.addSession(
		t, "global", sessionID,
		"/home/user/code/myapp", "Storage Archive",
		1704067200000, 1704067205000,
	)
	storage.addMessage(
		t, sessionID, "msg-u1", "user",
		1704067200000, nil,
	)
	storage.addTextPart(
		t, sessionID, "msg-u1", "part-u1",
		"hello storage", 1704067200000,
	)
	storage.addMessage(
		t, sessionID, "msg-a1", "assistant",
		1704067201000, nil,
	)
	storage.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"storage archive answer", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	err := os.RemoveAll(
		filepath.Join(env.opencodeDir, "storage"),
	)
	require.NoError(t, err, "remove storage tree")

	sqlite := createOpenCodeDB(t, env.opencodeDir)
	sqlite.addProject(t, "proj-1", "/home/user/code/myapp")
	sqlite.addSession(
		t, sessionID, "proj-1",
		1704067200000, 1704067205000,
	)
	sqlite.addMessage(
		t, "sqlite-msg-u1", sessionID, "user",
		1704067200000,
	)
	sqlite.addTextPart(
		t, "sqlite-part-u1", sessionID, "sqlite-msg-u1",
		"hello sqlite fallback", 1704067200000,
	)

	err = env.engine.SyncSingleSession(
		"opencode:" + sessionID,
	)
	require.NoError(t, err, "SyncSingleSession")

	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"hello storage", "storage archive answer",
	)
}

func TestOpenCodeSQLiteRootSyncPathsAndStaleReparse(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	const projectID = "proj-1"
	oc.addProject(t, projectID, "/home/user/code/myapp")

	const (
		timeCreated = int64(1704067200000)
		timeUpdated = int64(1704067205000)
	)
	seedTextSession := func(
		sessionID, question, answer string, offset int64,
	) string {
		t.Helper()
		created := timeCreated + offset
		updated := timeUpdated + offset
		oc.addSession(t, sessionID, projectID, created, updated)
		userMessageID := sessionID + "-msg-u1"
		oc.addMessage(t, userMessageID, sessionID, "user", created)
		oc.addTextPart(
			t, sessionID+"-part-u1", sessionID, userMessageID,
			question, created,
		)
		if answer == "" {
			return ""
		}
		assistantMessageID := sessionID + "-msg-a1"
		oc.addMessage(
			t, assistantMessageID, sessionID, "assistant", created+1,
		)
		oc.addTextPart(
			t, sessionID+"-part-a1", sessionID, assistantMessageID,
			answer, created+1,
		)
		return assistantMessageID
	}

	syncPathsID := "oc-sqlite-sync-paths"
	seedTextSession(
		syncPathsID,
		"original sqlite question", "original sqlite answer", 0,
	)
	staleVersionID := "oc-sqlite-stale-version"
	staleAssistantMessageID := seedTextSession(
		staleVersionID,
		"stale version question", "stale version answer", 10,
	)
	sourceMtimeID := "oc-source-sqlite"
	seedTextSession(
		sourceMtimeID,
		"source mtime question", "source mtime answer", 20,
	)
	goodSessionID := "oc-sqlite-watch-good"
	seedTextSession(
		goodSessionID,
		"good original question", "good original answer", 30,
	)
	badSessionID := "oc-sqlite-watch-bad"
	seedTextSession(
		badSessionID,
		"bad original question", "", 40,
	)

	initialMtime := env.engine.SourceMtime("opencode:" + sourceMtimeID)
	require.Equal(t, int64((timeUpdated+20)*1_000_000), initialMtime, "initial source mtime")

	sourceUpdated := timeUpdated + 1020
	oc.updateSessionTime(t, sourceMtimeID, sourceUpdated)
	updatedMtime := env.engine.SourceMtime("opencode:" + sourceMtimeID)
	require.Equal(t, sourceUpdated*1_000_000, updatedMtime, "updated source mtime")

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 5,
		Synced:        5,
		Skipped:       0,
	})

	oc.replaceTextContent(
		t, syncPathsID,
		"updated sqlite question",
		"updated sqlite answer",
		timeCreated,
	)
	oc.updateSessionTime(t, syncPathsID, timeUpdated+1000)

	env.engine.SyncPaths([]string{oc.path})

	assertMessageContent(
		t, env.db, "opencode:"+syncPathsID,
		"updated sqlite question",
		"updated sqlite answer",
	)

	oc.updateMessageData(t, staleAssistantMessageID, map[string]any{
		"role":    "assistant",
		"modelID": "claude-3-7-sonnet",
	})
	err := env.db.SetSessionDataVersion(
		"opencode:"+staleVersionID, 0,
	)
	require.NoError(t, err, "SetSessionDataVersion")

	stats := env.engine.SyncAll(context.Background(), nil)
	require.Equal(t, 0, stats.Failed, "SyncAll stats = %+v", stats)
	require.NotZero(t, stats.Synced, "SyncAll stats = %+v", stats)

	msgs := fetchMessages(t, env.db, "opencode:"+staleVersionID)
	assert.Equal(t, "claude-3-7-sonnet", msgs[1].Model)

	oc.replaceTextContent(
		t, goodSessionID,
		"good updated question",
		"good updated answer",
		timeCreated+30,
	)
	oc.updateSessionTime(t, goodSessionID, timeUpdated+1030)
	oc.updateSessionTime(t, badSessionID, timeUpdated+2040)
	oc.mustExec(
		t, "corrupt bad session message time",
		"UPDATE message SET time_created = ? WHERE id = ?",
		"broken-time", badSessionID+"-msg-u1",
	)

	env.engine.SyncPaths([]string{oc.path})

	assertMessageContent(
		t, env.db, "opencode:"+goodSessionID,
		"good updated question",
		"good updated answer",
	)
	assertMessageContent(
		t, env.db, "opencode:"+badSessionID,
		"bad original question",
	)
}

func TestSyncAllOpenCodeSQLiteFallbackPreservesStorageArchive(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	storage := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionID := "oc-sqlite-bulk-preserve"
	storage.addSession(
		t, "global", sessionID,
		"/home/user/code/myapp", "Storage Archive Bulk",
		1704067200000, 1704067205000,
	)
	storage.addMessage(
		t, sessionID, "msg-u1", "user",
		1704067200000, nil,
	)
	storage.addTextPart(
		t, sessionID, "msg-u1", "part-u1",
		"hello storage", 1704067200000,
	)
	storage.addMessage(
		t, sessionID, "msg-a1", "assistant",
		1704067201000, nil,
	)
	storage.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"storage archive answer", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	err := os.RemoveAll(
		filepath.Join(env.opencodeDir, "storage"),
	)
	require.NoError(t, err, "remove storage tree")

	sqlite := createOpenCodeDB(t, env.opencodeDir)
	sqlite.addProject(t, "proj-1", "/home/user/code/myapp")
	sqlite.addSession(
		t, sessionID, "proj-1",
		1704067200000, 1704067205000,
	)
	sqlite.addMessage(
		t, "sqlite-msg-u1", sessionID, "user",
		1704067200000,
	)
	sqlite.addTextPart(
		t, "sqlite-part-u1", sessionID, "sqlite-msg-u1",
		"hello sqlite fallback", 1704067200000,
	)

	stats := env.engine.SyncAll(context.Background(), nil)
	require.Equal(t, 0, stats.Failed, "stats.Failed = %d, want 0", stats.Failed)
	require.Equal(t, 0, stats.Synced, "stats.Synced = %d, want 0", stats.Synced)

	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"hello storage", "storage archive answer",
	)
}

func TestSyncPathsOpenCodeSQLiteDBEventIgnoresStaleSkipCache(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj-1", "/home/user/code/myapp")

	sessionID := "oc-sqlite-sync-paths-skip-cache"
	timeCreated := int64(1704067200000)
	timeUpdated := int64(1704067205000)

	oc.addSession(
		t, sessionID, "proj-1",
		timeCreated, timeUpdated,
	)
	oc.addMessage(
		t, "msg-u1", sessionID, "user", timeCreated,
	)
	oc.addMessage(
		t, "msg-a1", sessionID, "assistant", timeCreated+1,
	)
	oc.addTextPart(
		t, "part-u1", sessionID, "msg-u1",
		"original sqlite question", timeCreated,
	)
	oc.addTextPart(
		t, "part-a1", sessionID, "msg-a1",
		"original sqlite answer", timeCreated+1,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	info, err := os.Stat(oc.path)
	require.NoError(t, err, "stat opencode db")
	cachedMtime := info.ModTime()
	env.engine.InjectSkipCache(map[string]int64{
		oc.path: cachedMtime.UnixNano(),
	})

	oc.replaceTextContent(
		t, sessionID,
		"updated sqlite question",
		"updated sqlite answer",
		timeCreated,
	)
	oc.updateSessionTime(t, sessionID, timeUpdated+1000)
	require.NoError(t, os.Chtimes(oc.path, cachedMtime, cachedMtime), "restore db mtime")

	env.engine.SyncPaths([]string{oc.path})

	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"updated sqlite question",
		"updated sqlite answer",
	)
}

func TestSyncPathsOpenCodeStorageChildRetryWithoutSessionMtimeChange(
	t *testing.T,
) {

	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionPath := oc.addSession(
		t, "global", "oc-storage-retry",
		"/home/user/code/myapp", "Retry Session",
		1704067200000, 1704067205000,
	)
	messagePath := filepath.Join(
		env.opencodeDir, "storage", "message",
		"oc-storage-retry", "msg-u1.json",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(messagePath), 0o755), "mkdir message dir")
	err := os.WriteFile(
		messagePath, []byte(`{"id":"msg-u1"`), 0o644,
	)
	require.NoError(t, err, "write invalid message")

	env.engine.SyncPaths([]string{messagePath})
	sess, err := env.db.GetSession(
		context.Background(), "opencode:oc-storage-retry",
	)
	require.NoError(t, err, "GetSession")
	require.Nil(t, sess, "unexpected session after invalid child parse: %+v", sess)

	info, err := os.Stat(sessionPath)
	require.NoError(t, err, "stat session path")
	sessionMtime := info.ModTime().UnixNano()

	oc.addMessage(
		t, "oc-storage-retry", "msg-u1", "user",
		1704067200000, nil,
	)
	oc.addTextPart(
		t, "oc-storage-retry", "msg-u1", "part-u1",
		"hello after retry", 1704067200000,
	)
	err = os.Chtimes(
		sessionPath,
		time.Unix(0, sessionMtime),
		time.Unix(0, sessionMtime),
	)
	require.NoError(t, err, "restore session mtime")

	env.engine.SyncPaths([]string{messagePath})

	assertMessageContent(
		t, env.db, "opencode:oc-storage-retry",
		"hello after retry",
	)
}

func TestSyncPathsOpenCodeStorageChildUpdateAdvancesSessionMtime(
	t *testing.T,
) {

	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionPath := oc.addSession(
		t, "global", "oc-storage-mtime",
		"/home/user/code/myapp", "Mtime Session",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, "oc-storage-mtime", "msg-a1", "assistant",
		1704067201000, nil,
	)
	partPath := oc.addTextPart(
		t, "oc-storage-mtime", "msg-a1", "part-a1",
		"initial reply", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	_, initialMtime, ok := env.db.GetSessionFileInfo(
		"opencode:oc-storage-mtime",
	)
	require.True(t, ok, "expected initial session file_mtime")

	info, err := os.Stat(sessionPath)
	require.NoError(t, err, "stat session path")
	sessionMtime := info.ModTime().UnixNano()

	err = os.WriteFile(partPath, []byte(
		`{"id":"part-a1","sessionID":"oc-storage-mtime","messageID":"msg-a1","type":"text","text":"updated reply","time":{"created":1704067201000}}`,
	), 0o644)
	require.NoError(t, err, "rewrite part")
	err = os.Chtimes(
		sessionPath,
		time.Unix(0, sessionMtime),
		time.Unix(0, sessionMtime),
	)
	require.NoError(t, err, "restore session mtime")
	ocProvider, ok := parser.NewProvider(parser.AgentOpenCode, parser.ProviderConfig{
		Roots:   []string{env.opencodeDir},
		Machine: "local",
	})
	require.True(t, ok, "opencode provider available")
	ocSource, found, parseErr := ocProvider.FindSource(
		context.Background(),
		parser.FindSourceRequest{FullSessionID: "opencode:oc-storage-mtime"},
	)
	require.NoError(t, parseErr, "find opencode source after rewrite")
	require.True(t, found, "opencode source found after rewrite")
	ocOutcome, parseErr := ocProvider.Parse(context.Background(), parser.ParseRequest{
		Source:  ocSource,
		Machine: "local",
	})
	require.NoError(t, parseErr, "parse opencode source after rewrite")
	require.Len(t, ocOutcome.Results, 1, "parsed results after rewrite")
	parsedMsgs := ocOutcome.Results[0].Result.Messages
	require.Len(t, parsedMsgs, 1, "parsed messages after rewrite = %#v, want updated reply", parsedMsgs)
	require.Equal(t, "updated reply", parsedMsgs[0].Content,
		"parsed messages after rewrite = %#v, want updated reply", parsedMsgs)

	env.engine.SyncPaths([]string{partPath})

	_, updatedMtime, ok := env.db.GetSessionFileInfo(
		"opencode:oc-storage-mtime",
	)
	require.True(t, ok, "expected updated session file_mtime")
	require.Greater(t, updatedMtime, initialMtime, "updated file_mtime = %d, want > %d", updatedMtime, initialMtime)
	assertMessageContent(
		t, env.db, "opencode:oc-storage-mtime",
		"updated reply",
	)
}

func TestSourceMtimeOpenCodeStorageIncludesChildFiles(t *testing.T) {
	env := setupTestEnv(t)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionPath := oc.addSession(
		t, "global", "oc-source-mtime",
		"/home/user/code/myapp", "Source Mtime",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, "oc-source-mtime", "msg-a1", "assistant",
		1704067201000, nil,
	)
	partPath := oc.addTextPart(
		t, "oc-source-mtime", "msg-a1", "part-a1",
		"initial reply", 1704067201000,
	)

	initialMtime := env.engine.SourceMtime("opencode:oc-source-mtime")
	require.NotZero(t, initialMtime, "expected initial composite source mtime")

	info, err := os.Stat(sessionPath)
	require.NoError(t, err, "stat session path")
	sessionMtime := info.ModTime()
	future := time.Now().Add(2 * time.Second)

	err = os.WriteFile(partPath, []byte(
		`{"id":"part-a1","sessionID":"oc-source-mtime","messageID":"msg-a1","type":"text","text":"updated reply","time":{"created":1704067201000}}`,
	), 0o644)
	require.NoError(t, err, "rewrite part")
	require.NoError(t, os.Chtimes(partPath, future, future), "chtimes part")
	require.NoError(t, os.Chtimes(sessionPath, sessionMtime, sessionMtime), "restore session mtime")

	updatedMtime := env.engine.SourceMtime("opencode:oc-source-mtime")
	require.Greater(t, updatedMtime, initialMtime, "updated source mtime = %d, want > %d", updatedMtime, initialMtime)
}

func TestSourceMtimeOpenCodeStorageTracksChildRemoval(t *testing.T) {
	env := setupTestEnv(t)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	oc.addSession(
		t, "global", "oc-source-remove",
		"/home/user/code/myapp", "Source Remove",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, "oc-source-remove", "msg-a1", "assistant",
		1704067201000, nil,
	)
	partPath := oc.addTextPart(
		t, "oc-source-remove", "msg-a1", "part-a1",
		"initial reply", 1704067201000,
	)

	initialMtime := env.engine.SourceMtime("opencode:oc-source-remove")
	require.NotZero(t, initialMtime, "expected initial composite source mtime")

	partDir := filepath.Dir(partPath)
	require.NoError(t, os.Remove(partPath), "remove part")
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(partDir, future, future), "chtimes part dir")

	updatedMtime := env.engine.SourceMtime("opencode:oc-source-remove")
	require.Greater(t, updatedMtime, initialMtime, "updated source mtime = %d, want > %d", updatedMtime, initialMtime)
}

func TestSourceMtimeOpenCodeStorageTracksPartDirRemoval(t *testing.T) {
	env := setupTestEnv(t)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	oc.addSession(
		t, "global", "oc-source-remove-dir",
		"/home/user/code/myapp", "Source Remove Dir",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, "oc-source-remove-dir", "msg-a1", "assistant",
		1704067201000, nil,
	)
	partPath := oc.addTextPart(
		t, "oc-source-remove-dir", "msg-a1", "part-a1",
		"initial reply", 1704067201000,
	)

	initialMtime := env.engine.SourceMtime("opencode:oc-source-remove-dir")
	require.NotZero(t, initialMtime, "expected initial composite source mtime")

	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.RemoveAll(filepath.Dir(partPath)), "remove part dir")
	partRoot := filepath.Join(
		env.opencodeDir, "storage", "part",
	)
	require.NoError(t, os.Chtimes(partRoot, future, future), "chtimes part root")

	updatedMtime := env.engine.SourceMtime("opencode:oc-source-remove-dir")
	require.Greater(t, updatedMtime, initialMtime, "updated source mtime = %d, want > %d", updatedMtime, initialMtime)
}

func TestSourceMtimeOpenCodeStorageTracksMessageDirRemoval(
	t *testing.T,
) {
	env := setupTestEnv(t)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	oc.addSession(
		t, "global", "oc-source-remove-message-dir",
		"/home/user/code/myapp", "Source Remove Message Dir",
		1704067200000, 1704067205000,
	)
	messagePath := oc.addMessage(
		t, "oc-source-remove-message-dir", "msg-a1", "assistant",
		1704067201000, nil,
	)
	oc.addTextPart(
		t, "oc-source-remove-message-dir", "msg-a1", "part-a1",
		"initial reply", 1704067201000,
	)

	initialMtime := env.engine.SourceMtime(
		"opencode:oc-source-remove-message-dir",
	)
	require.NotZero(t, initialMtime, "expected initial composite source mtime")

	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.RemoveAll(filepath.Dir(messagePath)), "remove message dir")
	messageRoot := filepath.Join(
		env.opencodeDir, "storage", "message",
	)
	require.NoError(t, os.Chtimes(messageRoot, future, future), "chtimes message root")

	updatedMtime := env.engine.SourceMtime(
		"opencode:oc-source-remove-message-dir",
	)
	require.Greater(t, updatedMtime, initialMtime, "updated source mtime = %d, want > %d", updatedMtime, initialMtime)
}

func TestOpenCodeHybridRootSyncsSQLiteSessions(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	storage := createOpenCodeStorageFixture(t, env.opencodeDir)
	storage.addSession(
		t, "global", "oc-hybrid-storage",
		"/home/user/code/storage-app", "Hybrid Storage",
		1704067200000, 1704067205000,
	)
	storage.addMessage(
		t, "oc-hybrid-storage", "msg-a1", "assistant",
		1704067201000, nil,
	)
	storage.addTextPart(
		t, "oc-hybrid-storage", "msg-a1", "part-a1",
		"storage reply", 1704067201000,
	)

	const duplicateID = "oc-hybrid-dup"
	storage.addSession(
		t, "global", duplicateID,
		"/home/user/code/storage-app", "Hybrid Dup",
		1704067200000, 1704067205000,
	)
	storage.addMessage(
		t, duplicateID, "msg-storage-a1", "assistant",
		1704067201000, nil,
	)
	storage.addTextPart(
		t, duplicateID, "msg-storage-a1", "part-storage-a1",
		"canonical storage reply", 1704067201000,
	)

	sqlite := createOpenCodeDB(t, env.opencodeDir)
	sqlite.addProject(t, "proj-1", "/home/user/code/sqlite-app")
	sqlite.addProject(t, "proj-2", "/home/user/code/storage-app")
	sessionID := "oc-hybrid-sqlite"
	timeCreated := int64(1704067200000)
	timeUpdated := int64(1704067205000)
	sqlite.addSession(
		t, sessionID, "proj-1",
		timeCreated, timeUpdated,
	)
	sqlite.addMessage(
		t, "sqlite-msg-u1", sessionID, "user", timeCreated,
	)
	sqlite.addMessage(
		t, "sqlite-msg-a1", sessionID, "assistant", timeCreated+1,
	)
	sqlite.addTextPart(
		t, "sqlite-part-u1", sessionID, "sqlite-msg-u1",
		"original sqlite question", timeCreated,
	)
	sqlite.addTextPart(
		t, "sqlite-part-a1", sessionID, "sqlite-msg-a1",
		"original sqlite answer", timeCreated+1,
	)
	// The duplicate SQLite row is much newer so that the storage transcript
	// wins because of the duplicate-ID filter, not because of mtime ordering.
	duplicateUpdated := int64(1804067200000)
	sqlite.addSession(
		t, duplicateID, "proj-2",
		timeCreated, duplicateUpdated,
	)
	sqlite.addMessage(
		t, "dup-sqlite-msg-u1", duplicateID, "user", timeCreated,
	)
	sqlite.addMessage(
		t, "dup-sqlite-msg-a1", duplicateID, "assistant", timeCreated+1,
	)
	sqlite.addTextPart(
		t, "dup-sqlite-part-u1", duplicateID, "dup-sqlite-msg-u1",
		"stale sqlite question", timeCreated,
	)
	sqlite.addTextPart(
		t, "dup-sqlite-part-a1", duplicateID, "dup-sqlite-msg-a1",
		"stale sqlite answer", timeCreated+1,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 3,
		Synced:        3,
		Skipped:       0,
	})

	assertMessageContent(
		t, env.db, "opencode:oc-hybrid-storage",
		"storage reply",
	)
	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"original sqlite question",
		"original sqlite answer",
	)
	assertMessageContent(
		t, env.db, "opencode:"+duplicateID,
		"canonical storage reply",
	)

	virtualPath := parser.OpenCodeSQLiteVirtualPath(sqlite.path, sessionID)
	assert.Equal(t, virtualPath, env.engine.FindSourceFile("opencode:"+sessionID))
	assert.Equal(t, timeUpdated*1_000_000, env.engine.SourceMtime("opencode:"+sessionID))
	duplicateStoragePath := filepath.Join(
		env.opencodeDir, "storage", "session", "global",
		duplicateID+".json",
	)
	assert.Equal(t, duplicateStoragePath, env.engine.FindSourceFile("opencode:"+duplicateID))

	sqlite.replaceTextContent(
		t, sessionID,
		"updated by sync paths",
		"updated sqlite answer",
		timeCreated,
	)
	sqlite.updateSessionTime(t, sessionID, timeUpdated+1000)
	env.engine.SyncPaths([]string{sqlite.path})
	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"updated by sync paths",
		"updated sqlite answer",
	)

	sqlite.replaceTextContent(
		t, sessionID,
		"updated by single sync",
		"updated sqlite answer again",
		timeCreated,
	)
	sqlite.updateSessionTime(t, sessionID, timeUpdated+2000)
	require.NoError(t, env.engine.SyncSingleSession("opencode:"+sessionID), "SyncSingleSession")
	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"updated by single sync",
		"updated sqlite answer again",
	)

	sqlite.replaceTextContent(
		t, duplicateID,
		"newer stale sqlite question",
		"newer stale sqlite answer",
		timeCreated,
	)
	sqlite.updateSessionTime(t, duplicateID, duplicateUpdated+1000)
	env.engine.SyncPaths([]string{sqlite.path})
	assertMessageContent(
		t, env.db, "opencode:"+duplicateID,
		"canonical storage reply",
	)
}

// TestFindSourceFileSkipsHybridRootMissingSession covers the
// multi-root shadowing case: an early hybrid root with an
// opencode.db that lacks the requested session must not shadow a
// later pure-storage root that contains it. Without the
// session-existence gate in the OpenCode-format source lookup, the
// engine would return a virtual SQLite path pointing at the wrong DB.
func TestFindSourceFileSkipsHybridRootMissingSession(t *testing.T) {
	hybridRoot := t.TempDir()
	storageRoot := t.TempDir()
	err := os.MkdirAll(
		filepath.Join(hybridRoot, "storage", "session", "global"),
		0o755,
	)
	require.NoError(t, err, "mkdir hybrid storage")
	err = os.MkdirAll(
		filepath.Join(storageRoot, "storage", "session", "global"),
		0o755,
	)
	require.NoError(t, err, "mkdir storage root")

	hybridDB := createOpenCodeDB(t, hybridRoot)
	hybridDB.addProject(t, "proj-x", "/tmp/x")
	hybridDB.addSession(
		t, "oc-only-in-hybrid-db", "proj-x",
		1704067200000, 1704067205000,
	)

	const wantedID = "oc-real-in-storage"
	storage := createOpenCodeStorageFixture(t, storageRoot)
	storage.addSession(
		t, "global", wantedID,
		"/home/user/code/realapp", "Real Storage",
		1704067200000, 1704067205000,
	)
	storage.addMessage(
		t, wantedID, "msg-a1", "assistant",
		1704067201000, nil,
	)
	storage.addTextPart(
		t, wantedID, "msg-a1", "part-a1",
		"real storage reply", 1704067201000,
	)

	env := setupTestEnv(
		t,
		WithOpenCodeDirs([]string{hybridRoot, storageRoot}),
	)
	wantPath := filepath.Join(
		storageRoot, "storage", "session", "global",
		wantedID+".json",
	)
	require.Equal(t, wantPath, env.engine.FindSourceFile("opencode:"+wantedID), "FindSourceFile() = ..., want %q (hybrid root must not shadow)", wantPath)
}

func TestKiloStorageRewriteReplacesMessages(t *testing.T) {
	env := setupTestEnv(t)
	storage := createOpenCodeStorageFixture(t, env.kiloDir)
	const sessionID = "kilo-storage-rewrite"
	storage.addSession(
		t, "global", sessionID,
		"/home/user/code/kilo-app", "Kilo Rewrite",
		1704067200000, 1704067205000,
	)
	storage.addMessage(
		t, sessionID, "msg-a1", "assistant",
		1704067201000, nil,
	)
	partPath := storage.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"initial kilo reply", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
	})
	assertMessageContent(
		t, env.db, "kilo:"+sessionID, "initial kilo reply",
	)

	storage.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"rewritten kilo reply", 1704067201000,
	)
	changed := time.Unix(1804067200, 0)
	require.NoError(t, os.Chtimes(partPath, changed, changed))
	env.engine.SyncPaths([]string{partPath})

	assertMessageContent(
		t, env.db, "kilo:"+sessionID, "rewritten kilo reply",
	)
}

// TestMiMoCodeSessionDiffStorageWatcherEvents drives MiMoCode indexing
// purely through SyncPaths watcher events so the path classifier is the
// only route into the DB. The session JSON lives under
// storage/session_diff, so classification must use the resolved session
// subdir rather than the default storage/session. The follow-up part
// rewrite exercises the message/part classification branch, which
// resolves the session file under session_diff to advance the message.
func TestMiMoCodeSessionDiffStorageWatcherEvents(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentMiMoCode)
	storage := createMiMoCodeStorageFixture(t, env.mimocodeDir)
	const sessionID = "mimo-storage-watch"
	sessionPath := storage.addSession(
		t, "global", sessionID,
		"/home/user/code/mimo-app", "MiMoCode Watch",
		1704067200000, 1704067205000,
	)
	require.Contains(t, sessionPath,
		filepath.Join("storage", "session_diff"),
		"session JSON must live under storage/session_diff")
	storage.addMessage(
		t, sessionID, "msg-a1", "assistant",
		1704067201000, nil,
	)
	partPath := storage.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"initial mimo reply", 1704067201000,
	)

	// A session-JSON create event under storage/session_diff must
	// classify and index the session without any prior full sync.
	env.engine.SyncPaths([]string{sessionPath})
	assertSessionProject(t, env.db, "mimocode:"+sessionID, "mimo_app")
	assertMessageContent(
		t, env.db, "mimocode:"+sessionID, "initial mimo reply",
	)

	storage.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"rewritten mimo reply", 1704067201000,
	)
	changed := time.Unix(1804067200, 0)
	require.NoError(t, os.Chtimes(partPath, changed, changed))
	env.engine.SyncPaths([]string{partPath})

	assertMessageContent(
		t, env.db, "mimocode:"+sessionID, "rewritten mimo reply",
	)
}

// TestSyncPathsMiMoCodeStorageIgnoresStaleSessionSkipCache covers the
// shouldCacheSkip change: a MiMoCode session JSON under
// storage/session_diff must never be skip-cached by its own mtime,
// because its content depends on message/part files that change
// independently. With the session subdir hard-coded to storage/session,
// shouldCacheSkip would treat the session_diff file as cacheable, and a
// stale skip-cache entry at an unchanged session mtime would suppress a
// child-driven update.
func TestSyncPathsMiMoCodeStorageIgnoresStaleSessionSkipCache(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentMiMoCode)
	storage := createMiMoCodeStorageFixture(t, env.mimocodeDir)
	const sessionID = "mimo-storage-skip-cache"
	sessionPath := storage.addSession(
		t, "global", sessionID,
		"/home/user/code/mimo-app", "MiMoCode Skip Cache",
		1704067200000, 1704067205000,
	)
	storage.addMessage(
		t, sessionID, "msg-a1", "assistant",
		1704067201000, nil,
	)
	partPath := storage.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"initial mimo reply", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
	})
	assertMessageContent(
		t, env.db, "mimocode:"+sessionID, "initial mimo reply",
	)

	// Seed the skip cache for the session file at its current mtime.
	info, err := os.Stat(sessionPath)
	require.NoError(t, err, "stat session path")
	sessionMtime := info.ModTime()
	env.engine.InjectSkipCache(map[string]int64{
		sessionPath: sessionMtime.UnixNano(),
	})

	// Rewrite the part while holding the session JSON mtime constant,
	// so a stale skip-cache entry keyed on the session path would
	// suppress the update if session files were treated as cacheable.
	storage.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"rewritten mimo reply", 1704067201000,
	)
	require.NoError(t,
		os.Chtimes(sessionPath, sessionMtime, sessionMtime),
		"restore session mtime")

	env.engine.SyncPaths([]string{partPath})

	assertMessageContent(
		t, env.db, "mimocode:"+sessionID, "rewritten mimo reply",
	)
}

func TestKiloPreservesStorageArchiveAgainstSQLiteFallback(t *testing.T) {

	env := setupSingleAgentTestEnv(t, parser.AgentKilo)
	storage := createOpenCodeStorageFixture(t, env.kiloDir)
	const sessionID = "kilo-hybrid-preserve"
	storage.addSession(
		t, "global", sessionID,
		"/home/user/code/kilo-app", "Kilo Preserve",
		1704067200000, 1704067205000,
	)
	storage.addMessage(
		t, sessionID, "msg-storage-a1", "assistant",
		1704067201000, nil,
	)
	storage.addTextPart(
		t, sessionID, "msg-storage-a1", "part-storage-a1",
		"canonical kilo storage reply", 1704067201000,
	)

	sqlite := createKiloDB(t, env.kiloDir)
	sqlite.addProject(t, "proj-1", "/home/user/code/kilo-app")
	sqlite.addSession(
		t, sessionID, "proj-1",
		1704067200000, 1704067201000,
	)
	sqlite.addMessage(
		t, "sqlite-msg-a1", sessionID, "assistant",
		1704067201000,
	)
	sqlite.addTextPart(
		t, "sqlite-part-a1", sessionID, "sqlite-msg-a1",
		"older sqlite fallback reply", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
	})
	assertMessageContent(
		t, env.db, "kilo:"+sessionID,
		"canonical kilo storage reply",
	)

	storageSessionPath := filepath.Join(
		env.kiloDir, "storage", "session", "global",
		sessionID+".json",
	)
	require.NoError(t, os.Remove(storageSessionPath))
	env.engine.SyncPaths([]string{sqlite.path})

	assertMessageContent(
		t, env.db, "kilo:"+sessionID,
		"canonical kilo storage reply",
	)
}

func TestSyncAllSinceOpenCodeStoragePicksUpUsagePartUpdate(t *testing.T) {

	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionPath := oc.addSession(
		t, "global", "oc-since-usage-part",
		"/home/user/code/myapp", "Since Usage Part",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, "oc-since-usage-part", "msg-a1", "assistant",
		1704067201000, map[string]any{
			"modelID": "Gemini 3.5 Flash (High)",
		},
	)
	oc.addTextPart(
		t, "oc-since-usage-part", "msg-a1", "part-a1",
		"initial reply", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	cutoff := time.Now()
	info, err := os.Stat(sessionPath)
	require.NoError(t, err, "stat session path")
	sessionMtime := info.ModTime()
	future := cutoff.Add(2 * time.Second)

	usagePartPath := oc.writeJSON(t, filepath.Join(
		env.opencodeDir, "storage", "part", "msg-a1", "part-finish.json",
	), map[string]any{
		"id":        "part-finish",
		"sessionID": "oc-since-usage-part",
		"messageID": "msg-a1",
		"type":      "step-finish",
		"tokens": map[string]any{
			"input":  123,
			"output": 45,
			"cache": map[string]any{
				"read":  67,
				"write": 89,
			},
		},
		"time": map[string]any{
			"created": int64(1704067201000),
		},
	})
	require.NoError(t, os.Chtimes(usagePartPath, future, future), "chtimes usage part")
	require.NoError(t, os.Chtimes(sessionPath, sessionMtime, sessionMtime), "restore session mtime")

	// Composite freshness includes the part file, so the part-only edit is
	// fresh relative to the cutoff and re-syncs the updated reply.
	stats := env.engine.SyncAllSince(context.Background(), cutoff, nil)
	require.Equal(t, 1, stats.Synced, "SyncAllSince synced = %d, want 1", stats.Synced)

	daily, err := env.db.GetDailyUsage(context.Background(), db.UsageFilter{
		From:     "2024-01-01",
		To:       "2024-01-01",
		Timezone: "UTC",
	})
	require.NoError(t, err, "GetDailyUsage")
	require.Len(t, daily.Daily, 1, "daily rows")
	assert.Equal(t, 123, daily.Totals.InputTokens)
	assert.Equal(t, 45, daily.Totals.OutputTokens)
	assert.Equal(t, 67, daily.Totals.CacheReadTokens)
	assert.Equal(t, 89, daily.Totals.CacheCreationTokens)
	assert.Equal(t, []string{"Gemini 3.5 Flash (High)"}, daily.Daily[0].ModelsUsed)
}

// TestSyncAllOpenCodeStorageReparsesUnchangedSessionsIdempotently covers a
// re-sync of an unchanged OpenCode storage session. OpenCode is
// provider-authoritative, so a full SyncAll re-parses the source through
// the provider facade rather than taking the legacy DB-mtime skip; the
// re-parse must be idempotent and keep the same content.
func TestSyncAllOpenCodeStorageReparsesUnchangedSessionsIdempotently(t *testing.T) {

	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	oc.addSession(
		t, "global", "oc-skip-unchanged",
		"/home/user/code/myapp", "Skip Unchanged",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, "oc-skip-unchanged", "msg-a1", "assistant",
		1704067201000, nil,
	)
	oc.addTextPart(
		t, "oc-skip-unchanged", "msg-a1", "part-a1",
		"stable reply", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	stats := env.engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.TotalSessions, "SyncAll stats = %+v", stats)
	require.Equal(t, 0, stats.Failed, "SyncAll stats = %+v", stats)
	assertMessageContent(
		t, env.db, "opencode:oc-skip-unchanged", "stable reply",
	)
}

func TestSyncAllOpenCodeStorageMissingMessagePreservesArchive(t *testing.T) {

	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionPath := oc.addSession(
		t, "global", "oc-missing-message",
		"/home/user/code/myapp", "Missing Message",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, "oc-missing-message", "msg-u1", "user",
		1704067200000, nil,
	)
	oc.addTextPart(
		t, "oc-missing-message", "msg-u1", "part-u1",
		"question", 1704067200000,
	)
	messagePath := oc.addMessage(
		t, "oc-missing-message", "msg-a1", "assistant",
		1704067201000, nil,
	)
	oc.addTextPart(
		t, "oc-missing-message", "msg-a1", "part-a1",
		"answer", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	require.NoError(t, os.Remove(messagePath), "remove message file")
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(sessionPath, future, future), "touch session path")

	env.engine.SyncAll(context.Background(), nil)

	assertMessageContent(
		t, env.db, "opencode:oc-missing-message",
		"question", "answer",
	)
}

func TestSyncAllOpenCodeStoragePreservesLegacySQLiteArchive(
	t *testing.T,
) {

	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	sqlite := createOpenCodeDB(t, env.opencodeDir)
	sqlite.addProject(t, "proj-1", "/home/user/code/myapp")

	sessionID := "oc-storage-upgrade-legacy"
	timeCreated := int64(1704067200000)
	timeUpdated := int64(1704067205000)

	sqlite.addSession(
		t, sessionID, "proj-1",
		timeCreated, timeUpdated,
	)
	sqlite.addMessage(
		t, "msg-u1", sessionID, "user", timeCreated,
	)
	sqlite.addMessage(
		t, "msg-a1", sessionID, "assistant", timeCreated+1,
	)
	sqlite.addTextPart(
		t, "part-u1", sessionID, "msg-u1",
		"legacy sqlite question", timeCreated,
	)
	sqlite.addTextPart(
		t, "part-a1", sessionID, "msg-a1",
		"legacy sqlite answer", timeCreated+1,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	storage := createOpenCodeStorageFixture(t, env.opencodeDir)
	storage.addSession(
		t, "global", sessionID,
		"/home/user/code/myapp", "Storage Upgrade",
		timeCreated, timeUpdated+1000,
	)
	storage.addMessage(
		t, sessionID, "msg-u1", "user",
		timeCreated, nil,
	)
	storage.addTextPart(
		t, sessionID, "msg-u1", "part-u1",
		"legacy sqlite question", timeCreated,
	)

	env.engine.SyncAll(context.Background(), nil)

	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"legacy sqlite question",
		"legacy sqlite answer",
	)
}

func TestSyncAllOpenCodeStorageMissingPartDirPreservesArchive(t *testing.T) {

	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionPath := oc.addSession(
		t, "global", "oc-missing-part",
		"/home/user/code/myapp", "Missing Part",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, "oc-missing-part", "msg-u1", "user",
		1704067200000, nil,
	)
	partPath := oc.addTextPart(
		t, "oc-missing-part", "msg-u1", "part-u1",
		"question", 1704067200000,
	)
	oc.addMessage(
		t, "oc-missing-part", "msg-a1", "assistant",
		1704067201000, nil,
	)
	oc.addTextPart(
		t, "oc-missing-part", "msg-a1", "part-a1",
		"answer", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	require.NoError(t, os.RemoveAll(filepath.Dir(partPath)), "remove part dir")
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(sessionPath, future, future), "touch session path")

	stats := env.engine.SyncAll(context.Background(), nil)
	require.Equal(t, 0, stats.Failed, "stats.Failed = %d, want 0", stats.Failed)
	require.Equal(t, 0, stats.Synced, "stats.Synced = %d, want 0", stats.Synced)

	assertMessageContent(
		t, env.db, "opencode:oc-missing-part",
		"question", "answer",
	)
}

func TestSyncSingleSessionOpenCodeStorageMissingMessagePreservesArchive(
	t *testing.T,
) {

	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionID := "oc-missing-message-single"
	sessionPath := oc.addSession(
		t, "global", sessionID,
		"/home/user/code/myapp", "Missing Message Single",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, sessionID, "msg-u1", "user",
		1704067200000, nil,
	)
	oc.addTextPart(
		t, sessionID, "msg-u1", "part-u1",
		"question", 1704067200000,
	)
	messagePath := oc.addMessage(
		t, sessionID, "msg-a1", "assistant",
		1704067201000, nil,
	)
	oc.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"answer", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	require.NoError(t, os.Remove(messagePath), "remove message file")
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(sessionPath, future, future), "touch session path")

	err := env.engine.SyncSingleSession(
		"opencode:" + sessionID,
	)
	require.NoError(t, err, "SyncSingleSession")

	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"question", "answer",
	)
}

func TestSyncSingleSessionOpenCodeStoragePreservedUpdateDoesNotEmit(
	t *testing.T,
) {
	em := &fakeEmitter{}
	env := setupTestEnv(t, WithEmitter(em))
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionID := "oc-missing-message-no-emit"
	sessionPath := oc.addSession(
		t, "global", sessionID,
		"/home/user/code/myapp", "Missing Message No Emit",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, sessionID, "msg-u1", "user",
		1704067200000, nil,
	)
	oc.addTextPart(
		t, sessionID, "msg-u1", "part-u1",
		"question", 1704067200000,
	)
	messagePath := oc.addMessage(
		t, sessionID, "msg-a1", "assistant",
		1704067201000, nil,
	)
	oc.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"answer", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	em.mu.Lock()
	em.scopes = em.scopes[:0]
	em.mu.Unlock()

	require.NoError(t, os.Remove(messagePath), "remove message file")
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(sessionPath, future, future), "touch session path")

	err := env.engine.SyncSingleSession(
		"opencode:" + sessionID,
	)
	require.NoError(t, err, "SyncSingleSession")

	assert.Empty(t, em.got(), "expected no emissions for preserved update")
	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"question", "answer",
	)
}

func TestSyncPathsOpenCodeStorageMissingMessagePreservesArchive(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionID := "oc-missing-message-paths"
	oc.addSession(
		t, "global", sessionID,
		"/home/user/code/myapp", "Missing Message Paths",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, sessionID, "msg-u1", "user",
		1704067200000, nil,
	)
	oc.addTextPart(
		t, sessionID, "msg-u1", "part-u1",
		"question", 1704067200000,
	)
	messagePath := oc.addMessage(
		t, sessionID, "msg-a1", "assistant",
		1704067201000, nil,
	)
	oc.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"answer", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	require.NoError(t, os.Remove(messagePath), "remove message file")

	env.engine.SyncPaths([]string{messagePath})

	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"question", "answer",
	)
}

func TestSyncPathsOpenCodeStoragePreservedUpdateDoesNotEmitOrCountSynced(
	t *testing.T,
) {
	em := &fakeEmitter{}
	env := setupTestEnv(t, WithEmitter(em))
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionID := "oc-missing-message-paths-no-emit"
	oc.addSession(
		t, "global", sessionID,
		"/home/user/code/myapp", "Missing Message Paths No Emit",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, sessionID, "msg-u1", "user",
		1704067200000, nil,
	)
	oc.addTextPart(
		t, sessionID, "msg-u1", "part-u1",
		"question", 1704067200000,
	)
	messagePath := oc.addMessage(
		t, sessionID, "msg-a1", "assistant",
		1704067201000, nil,
	)
	oc.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"answer", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	em.mu.Lock()
	em.scopes = em.scopes[:0]
	em.mu.Unlock()

	require.NoError(t, os.Remove(messagePath), "remove message file")

	env.engine.SyncPaths([]string{messagePath})

	assert.Empty(t, em.got(), "expected no emissions for preserved SyncPaths update")
	stats := env.engine.LastSyncStats()
	require.Equal(t, 0, stats.Synced, "LastSyncStats().Synced = %d, want 0", stats.Synced)
	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"question", "answer",
	)
}

func TestSyncPathsOpenCodeStorageMissingPartDirPreservesArchive(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionID := "oc-missing-part-paths"
	oc.addSession(
		t, "global", sessionID,
		"/home/user/code/myapp", "Missing Part Paths",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, sessionID, "msg-u1", "user",
		1704067200000, nil,
	)
	oc.addTextPart(
		t, sessionID, "msg-u1", "part-u1",
		"question", 1704067200000,
	)
	messagePath := oc.addMessage(
		t, sessionID, "msg-a1", "assistant",
		1704067201000, nil,
	)
	partPath := oc.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"answer", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	require.NoError(t, os.RemoveAll(filepath.Dir(partPath)), "remove part dir")

	env.engine.SyncPaths([]string{messagePath})

	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"question", "answer",
	)
}

func TestSyncSingleSessionOpenCodeStorageMissingPartPreservesArchive(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionID := "oc-missing-part-single"
	sessionPath := oc.addSession(
		t, "global", sessionID,
		"/home/user/code/myapp", "Missing Part Single",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, sessionID, "msg-a1", "assistant",
		1704067201000, nil,
	)
	part1Path := oc.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"first part", 1704067201000,
	)
	oc.addTextPart(
		t, sessionID, "msg-a1", "part-a2",
		"second part", 1704067201001,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	require.NoError(t, os.Remove(part1Path), "remove part file")
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(sessionPath, future, future), "touch session path")

	err := env.engine.SyncSingleSession(
		"opencode:" + sessionID,
	)
	require.NoError(t, err, "SyncSingleSession")

	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"first part\nsecond part",
	)
}

func TestSyncAllOpenCodeStorageContentRewritePreservesArchive(
	t *testing.T,
) {
	env := setupTestEnv(t)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionID := "oc-content-rewrite"
	sessionPath := oc.addSession(
		t, "global", sessionID,
		"/home/user/code/myapp", "Content Rewrite",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, sessionID, "msg-u1", "user",
		1704067200000, nil,
	)
	partPath := oc.addTextPart(
		t, sessionID, "msg-u1", "part-u1",
		"complete response", 1704067200000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	dbtest.WriteTestFile(t, partPath, []byte(
		`{"id":"part-u1","sessionID":"`+sessionID+`","messageID":"msg-u1","type":"text","text":"cut","time":{"created":1704067200000}}`,
	))
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(sessionPath, future, future), "touch session path")

	env.engine.SyncAll(context.Background(), nil)

	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"complete response",
	)
}

func TestSyncAllOpenCodeStorageMissingStepFinishPreservesTokens(
	t *testing.T,
) {
	env := setupTestEnv(t)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionID := "oc-missing-step-finish"
	sessionPath := oc.addSession(
		t, "global", sessionID,
		"/home/user/code/myapp", "Missing Step Finish",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, sessionID, "msg-a1", "assistant",
		1704067201000, map[string]any{
			"modelID": "gpt-5.2-codex",
		},
	)
	oc.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"answer", 1704067201000,
	)
	stepFinishPath := oc.writeJSON(t, filepath.Join(
		env.opencodeDir, "storage", "part", "msg-a1", "part-a2.json",
	), map[string]any{
		"id":        "part-a2",
		"sessionID": sessionID,
		"messageID": "msg-a1",
		"type":      "step-finish",
		"tokens": map[string]any{
			"input":  300,
			"output": 200,
		},
		"time": map[string]any{
			"created": 1704067201001,
		},
	})

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	require.NoError(t, os.Remove(stepFinishPath), "remove step-finish part")
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(sessionPath, future, future), "touch session path")

	env.engine.SyncAll(context.Background(), nil)

	full, err := env.db.GetSessionFull(
		context.Background(), "opencode:"+sessionID,
	)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, full, "session missing after preserve")
	require.True(t, full.HasTotalOutputTokens, "session output tokens = (%v, %d), want (true, 200)", full.HasTotalOutputTokens, full.TotalOutputTokens)
	require.Equal(t, 200, full.TotalOutputTokens, "session output tokens = (%v, %d), want (true, 200)", full.HasTotalOutputTokens, full.TotalOutputTokens)
	require.True(t, full.HasPeakContextTokens, "session context tokens = (%v, %d), want (true, 300)", full.HasPeakContextTokens, full.PeakContextTokens)
	require.Equal(t, 300, full.PeakContextTokens, "session context tokens = (%v, %d), want (true, 300)", full.HasPeakContextTokens, full.PeakContextTokens)

	msgs := fetchMessages(t, env.db, "opencode:"+sessionID)
	require.Len(t, msgs, 1, "len(msgs) = %d, want 1", len(msgs))
	require.True(t, msgs[0].HasOutputTokens, "message output tokens = (%v, %d), want (true, 200)", msgs[0].HasOutputTokens, msgs[0].OutputTokens)
	require.Equal(t, 200, msgs[0].OutputTokens, "message output tokens = (%v, %d), want (true, 200)", msgs[0].HasOutputTokens, msgs[0].OutputTokens)
	require.True(t, msgs[0].HasContextTokens, "message context tokens = (%v, %d), want (true, 300)", msgs[0].HasContextTokens, msgs[0].ContextTokens)
	require.Equal(t, 300, msgs[0].ContextTokens, "message context tokens = (%v, %d), want (true, 300)", msgs[0].HasContextTokens, msgs[0].ContextTokens)
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

	env.engine.SyncAll(context.Background(), nil)

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

	env.engine.SyncAll(context.Background(), nil)

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
		env.engine.SyncAll(context.Background(), syncProgress)
	}()

	// Wait until SyncAll is inside the locked section.
	<-syncAllEntered

	go func() {
		defer wg.Done()
		env.engine.ResyncAll(context.Background(), resyncProgress)
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
		assert.Equal(t, 1, sess.UserMessageCount, "user_message_count = %d, want 1", sess.UserMessageCount)
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
	env.engine.SyncAll(context.Background(), nil)

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
	require.NoError(t, err, "corrupt counts")

	err = env.engine.SyncSingleSession(
		"filter-single",
	)
	require.NoError(t, err, "SyncSingleSession")

	// Counts should be corrected by writeSessionFull.
	assertSessionMessageCount(t, env.db, "filter-single", 3)
	assertSessionState(t, env.db, "filter-single", func(sess *db.Session) {
		assert.Equal(t, 1, sess.UserMessageCount, "user_message_count = %d, want 1", sess.UserMessageCount)
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
	assert.NotEmpty(t, src, "FindSourceFile failed for sess2 in second directory")
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
			assert.Equal(t, "cursor", sess.Agent, "agent = %q, want cursor", sess.Agent)
		},
	)
	assertSessionState(
		t, env.db, "cursor:sess2",
		func(sess *db.Session) {
			assert.Equal(t, "cursor", sess.Agent, "agent = %q, want cursor", sess.Agent)
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
	assert.NotEmpty(t, src, "FindSourceFile failed for cursor:sess2 "+"in second directory")
}

func TestSyncPathsCursorNestedLayout(t *testing.T) {
	env := setupTestEnv(t)

	path := env.writeNestedCursorSession(
		t, env.cursorDir,
		"Users-alice-code-nested-proj",
		"nested-sync", ".jsonl",
		"user:\nHello nested cursor\nassistant:\nHi there!\n",
	)

	env.engine.SyncPaths([]string{path})

	assertSessionProject(
		t, env.db, "cursor:nested-sync", "nested_proj",
	)
	assertSessionMessageCount(
		t, env.db, "cursor:nested-sync", 2,
	)
}

func TestSyncSingleSessionCursorNestedLayoutPreservesProject(
	t *testing.T,
) {
	env := setupTestEnv(t)

	path := env.writeNestedCursorSession(
		t, env.cursorDir,
		"Users-alice-code-nested-proj",
		"nested-resync", ".jsonl",
		"user:\nHello nested cursor\nassistant:\nHi there!\n",
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1, Skipped: 0,
	})
	assertSessionProject(
		t, env.db, "cursor:nested-resync", "nested_proj",
	)

	updated := "user:\nHello nested cursor\n" +
		"assistant:\nHi there!\n" +
		"user:\nFollow-up\n" +
		"assistant:\nGot it.\n"
	require.NoError(t, os.WriteFile(path, []byte(updated), 0o644), "WriteFile")

	err := env.engine.SyncSingleSession(
		"cursor:nested-resync",
	)
	require.NoError(t, err, "SyncSingleSession")

	assertSessionProject(
		t, env.db, "cursor:nested-resync", "nested_proj",
	)
	assertSessionMessageCount(
		t, env.db, "cursor:nested-resync", 4,
	)
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
		require.NotNil(t, sess.ParentSessionID, "fork parent = nil, want parent-uuid")
		assert.Equal(t, "parent-uuid", *sess.ParentSessionID, "fork parent = %v, want parent-uuid", sess.ParentSessionID)
		assert.Equal(t, "fork", sess.RelationshipType, "fork relationship_type = %q, want fork", sess.RelationshipType)
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
	require.Equal(t, 2, len(msgs), "got %d messages, want 2", len(msgs))

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
	require.NoError(t, err, "update message content")

	// Capture FTS state before resync so a regression that
	// breaks FTS isn't masked by HasFTS() returning false
	// post-resync.
	hadFTS := env.db.HasFTS()

	// ResyncAll should re-parse and replace message content. Gemini is
	// provider-authoritative, so it has no DB-backed mtime skip; a plain
	// SyncAll would also re-parse the unchanged file. ResyncAll additionally
	// drops and rebuilds the FTS index, which is what this test guards.
	env.engine.ResyncAll(context.Background(), nil)
	msgs = fetchMessages(t, env.db, fullID)
	require.Equal(t, 2, len(msgs), "got %d messages after resync, want 2", len(msgs))
	assert.NotContains(t, msgs[1].Content, "stale content", "ResyncAll did not replace message content")
	assert.Contains(t, msgs[1].Content, "Here is the explanation.",
		"unexpected content after resync: %q", msgs[1].Content)

	// FTS search should work after resync (index was dropped
	// and rebuilt).
	if hadFTS {
		require.True(t, env.db.HasFTS(), "FTS available before resync but not after")
		page, err := env.db.Search(
			context.Background(),
			db.SearchFilter{Query: "explanation"},
		)
		require.NoError(t, err, "search after resync")
		assert.NotZero(t, len(page.Results), "FTS search returned no results after resync")
	}
}

func TestResyncAllPreservesTrashedSessionData(t *testing.T) {
	env := setupTestEnv(t)

	original := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "original trashed prompt").
		AddClaudeAssistant(tsZeroS5, "original trashed reply").
		String()
	path := env.writeClaudeSession(
		t, "test-proj", "resync-trash.jsonl", original,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})
	orphanContent := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "orphan prompt").
		AddClaudeAssistant(tsZeroS5, "orphan reply").
		String()
	orphanPath := env.writeClaudeSession(
		t, "test-proj", "active-orphan.jsonl", orphanContent,
	)
	env.engine.SyncPaths([]string{orphanPath})
	assertSessionMessageCount(t, env.db, "active-orphan", 2)
	require.NoError(t, env.db.UpdateSessionSignals(
		"active-orphan",
		db.SessionSignalUpdate{
			Outcome:           "completed",
			OutcomeConfidence: "high",
			EndedWithRole:     "assistant",
			HealthScore:       new(94),
			HealthGrade:       new("A"),
			QualitySignals: db.QualitySignals{
				Version:                     db.CurrentQualitySignalVersion,
				ShortPromptCount:            1,
				MissingSuccessCriteriaCount: 1,
			},
		},
	), "UpdateSessionSignals orphan")
	require.NoError(t, os.Remove(orphanPath), "remove orphan source")

	require.NoError(t, env.db.SoftDeleteSession("resync-trash"), "SoftDeleteSession")

	replacement := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "replacement prompt").
		AddClaudeAssistant(tsZeroS5, "replacement reply").
		String()
	dbtest.WriteTestFile(t, path, []byte(replacement))

	stats := env.engine.ResyncAll(context.Background(), nil)
	require.False(t, stats.Aborted, "ResyncAll aborted: %+v", stats)
	assertSessionMessageCount(t, env.db, "active-orphan", 2)
	assertSessionState(t, env.db, "active-orphan", func(sess *db.Session) {
		if sess.HealthScore == nil || *sess.HealthScore != 94 {
			t.Fatalf("orphan health score = %v, want 94", sess.HealthScore)
		}
		qs := sess.StoredQualitySignals()
		if qs == nil {
			t.Fatal("orphan quality signals were not preserved")
		}
		if qs.Version != db.CurrentQualitySignalVersion ||
			qs.ShortPromptCount != 1 ||
			qs.MissingSuccessCriteriaCount != 1 {
			t.Fatalf("orphan quality signals = %+v, want preserved prompt signals", qs)
		}
	})

	full, err := env.db.GetSessionFull(
		context.Background(), "resync-trash",
	)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, full, "trashed session was not preserved as trashed")
	require.NotNil(t, full.DeletedAt, "trashed session was not preserved as trashed")
	msgs := fetchMessages(t, env.db, "resync-trash")
	require.Equal(t, 2, len(msgs), "messages = %d, want 2", len(msgs))
	require.Equal(t, "original trashed prompt", msgs[0].Content, "trashed content = %q, want original content", msgs[0].Content)
}

// TestResyncAllSurfacesQueuedCommands locks in that bumping
// dataVersion (which forces a full resync) recovers Claude
// queued_command attachments dropped by older parser versions.
// Old DBs synced before the parser fix have no row for the
// mid-flight user message; ResyncAll must replay the file and
// reinstate it.
func TestResyncAllSurfacesQueuedCommands(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("first", tsEarly),
		testjsonl.ClaudeAssistantJSON([]map[string]any{
			{"type": "text", "text": "starting"},
		}, tsEarlyS1),
		testjsonl.ClaudeQueuedCommandJSON(
			"also do X", "2024-01-01T10:00:02Z",
		),
		testjsonl.ClaudeAssistantJSON([]map[string]any{
			{"type": "text", "text": "done"},
		}, tsEarlyS5),
	)

	env.writeClaudeSession(
		t, "test-proj", "queued-resync.jsonl", content,
	)

	// Initial sync uses the current parser, which surfaces the
	// queued_command as message ordinal 2.
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	const sessionID = "queued-resync"
	msgs := fetchMessages(t, env.db, sessionID)
	require.Equal(t, 4, len(msgs), "initial sync: got %d messages, want 4", len(msgs))

	// Simulate an old-parser DB by removing the queued_command
	// row directly. Older versions of the parser would never
	// have stored it.
	err := env.db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"DELETE FROM messages WHERE session_id = ?"+
				" AND source_subtype = 'queued_command'",
			sessionID,
		)
		return err
	})
	require.NoError(t, err, "delete queued_command row")
	msgs = fetchMessages(t, env.db, sessionID)
	require.Equal(t, 3, len(msgs), "after stale simulation: got %d, want 3", len(msgs))

	// SyncAll must NOT recover the dropped row: the source
	// file is unchanged on disk, so the engine skips it.
	stats := env.engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Skipped, "SyncAll: expected Skipped=1, got %d", stats.Skipped)
	msgs = fetchMessages(t, env.db, sessionID)
	require.Equal(t, 3, len(msgs), "after SyncAll: got %d, want 3", len(msgs))

	// ResyncAll re-parses every session from scratch and the
	// queued_command reappears.
	env.engine.ResyncAll(context.Background(), nil)

	msgs = fetchMessages(t, env.db, sessionID)
	require.Equal(t, 4, len(msgs), "after ResyncAll: got %d, want 4", len(msgs))

	var queued *db.Message
	for i := range msgs {
		if msgs[i].SourceSubtype == "queued_command" {
			queued = &msgs[i]
			break
		}
	}
	require.NotNil(t, queued, "ResyncAll did not restore queued_command row")
	assert.Equal(t, "also do X", queued.Content, "queued_command content = %q, want %q", queued.Content, "also do X")
	assert.Equal(t, "user", queued.Role, "queued_command role = %q, want user", queued.Role)
	assert.False(t, queued.IsSystem, "queued_command should not be is_system=true")
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

	env.engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, env.db, "insight-test", 2)

	// Insert an insight into the DB.
	_, err := env.db.InsertInsight(db.Insight{
		Type:     "daily_activity",
		DateFrom: "2025-01-15",
		DateTo:   "2025-01-15",
		Agent:    "claude",
		Content:  "test insight survives resync",
	})
	require.NoError(t, err, "InsertInsight")

	// ResyncAll should rebuild sessions and preserve
	// insights.
	stats := env.engine.ResyncAll(context.Background(), nil)
	require.NotZero(t, stats.Synced, "expected at least 1 synced session")

	assertSessionMessageCount(t, env.db, "insight-test", 2)

	insights, err := env.db.ListInsights(
		context.Background(), db.InsightFilter{},
	)
	require.NoError(t, err, "ListInsights")
	require.Equal(t, 1, len(insights), "got %d insights, want 1", len(insights))
	assert.Equal(t, "test insight survives resync", insights[0].Content, "insight content = %q, want preserved", insights[0].Content)
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

	env.engine.SyncAll(context.Background(), nil)
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
	require.NoError(t, os.Chmod(sessionPath, 0), "chmod")
	t.Cleanup(func() {
		os.Chmod(sessionPath, 0o644)
	})

	stats := env.engine.ResyncAll(context.Background(), nil)

	require.NotEqual(t, 0, stats.Failed, "expected failures, got 0")
	require.NotZero(t, stats.TotalSessions, "expected TotalSessions > 0")

	assert.True(t, stats.Aborted, "expected Aborted = true")

	hasAbortWarning := false
	for _, w := range stats.Warnings {
		if strings.Contains(w, "resync aborted") {
			hasAbortWarning = true
		}
	}
	assert.True(t, hasAbortWarning, "expected 'resync aborted' warning")

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
	env.engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, env.db, "forked", 10)
	assertSessionMessageCount(t, env.db, "forked-i", 2)

	// Make both normal files unreadable.
	for _, name := range []string{"bad1.jsonl", "bad2.jsonl"} {
		p := filepath.Join(env.claudeDir, "proj", name)
		require.NoError(t, os.Chmod(p, 0), "chmod %s", name)
		t.Cleanup(func() { os.Chmod(p, 0o644) })
	}

	stats := env.engine.ResyncAll(context.Background(), nil)

	// Expect: filesOK=1, Failed=2, Synced=2.
	// Abort should fire because Failed(2) > filesOK(1).
	require.Equal(t, 2, stats.Failed, "Failed = %d, want 2", stats.Failed)
	require.Equal(t, 2, stats.Synced, "Synced = %d, want 2", stats.Synced)

	hasAbortWarning := false
	for _, w := range stats.Warnings {
		if strings.Contains(w, "resync aborted") {
			hasAbortWarning = true
		}
	}
	assert.True(t, hasAbortWarning, "expected abort: Failed(2) > filesOK(1) "+"should trigger even though Failed == Synced")

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
	stats := env.engine.ResyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Synced, "resync: synced = %d, want 1", stats.Synced)
	assert.False(t, stats.Aborted, "unexpected Aborted = true on successful resync")
	for _, w := range stats.Warnings {
		assert.Fail(t, "unexpected warning", w)
	}

	// Verify reads work on the reopened DB.
	s, err := env.db.GetSession(
		context.Background(), "avail",
	)
	require.NoError(t, err, "GetSession after resync")
	require.NotNil(t, s, "session missing after resync")

	msgs := fetchMessages(t, env.db, "avail")
	require.Equal(t, 2, len(msgs), "got %d messages, want 2", len(msgs))

	// Verify writes work on the reopened DB.
	err = env.db.UpsertSession(db.Session{
		ID:           "post-resync-write",
		Project:      "avail-proj",
		Machine:      "local",
		Agent:        "claude",
		MessageCount: 1,
	})
	require.NoError(t, err, "UpsertSession after resync")
	s2, err := env.db.GetSession(
		context.Background(), "post-resync-write",
	)
	require.NoError(t, err, "GetSession post-write")
	require.NotNil(t, s2, "session written after resync not found")

	// Verify a subsequent SyncAll still works (engine state
	// is consistent with the reopened DB).
	stats2 := env.engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 0, stats2.Synced, "post-resync SyncAll: synced=%d skipped=%d", stats2.Synced, stats2.Skipped)
	assert.Equal(t, 1, stats2.Skipped, "post-resync SyncAll: synced=%d skipped=%d", stats2.Synced, stats2.Skipped)
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

	env.engine.SyncAll(context.Background(), nil)

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
	stats := env.engine.ResyncAll(context.Background(), nil)
	cancel()
	wg.Wait()

	require.Equal(t, 1, stats.Synced, "resync: synced = %d, want 1", stats.Synced)

	// Post-resync reads must succeed.
	s, err := env.db.GetSession(
		context.Background(), "conc",
	)
	require.NoError(t, err, "GetSession after resync")
	require.NotNil(t, s, "session missing after resync")
}

// TestResyncAllAbortsOnEmptyDiscovery verifies that resync does
// not replace a populated DB with an empty one when discovery
// returns zero files (e.g. session directories are temporarily
// inaccessible or misconfigured).
func TestResyncAllAbortsOnEmptyDiscovery(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	// Seed existing data via initial sync.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "keep me").
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()
	env.writeClaudeSession(t, "proj", "keep.jsonl", content)
	env.engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, env.db, "keep", 2)

	// Remove all session files to simulate empty discovery.
	entries, err := os.ReadDir(
		filepath.Join(env.claudeDir, "proj"),
	)
	require.NoError(t, err, "reading dir")
	for _, e := range entries {
		p := filepath.Join(env.claudeDir, "proj", e.Name())
		os.Remove(p)
	}

	stats := env.engine.ResyncAll(context.Background(), nil)

	// Swap must be aborted.
	hasAbortWarning := false
	for _, w := range stats.Warnings {
		if strings.Contains(w, "resync aborted") {
			hasAbortWarning = true
		}
	}
	assert.True(t, hasAbortWarning, "expected abort when discovery returns zero files "+"but old DB has sessions")

	// Original data must be preserved.
	assertSessionMessageCount(t, env.db, "keep", 2)
}

// TestResyncAllKiroSQLiteOnly verifies that current-store Kiro
// sessions do not trip the empty-discovery guard simply because
// they are DB-backed rather than JSONL-backed.
func TestResyncAllKiroSQLiteOnly(t *testing.T) {

	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	ks := createKiroSQLiteDB(t, env.kiroDir)
	ks.addSession(
		t, "/home/user/code/kiro-app", "kiro-resync-only",
		readKiroSQLiteFixture(t, "standard_payload.json"),
		1779012000000, 1779012030000,
	)

	env.engine.SyncAll(context.Background(), nil)
	agentviewID := "kiro:kiro-resync-only"
	assertSessionMessageCount(t, env.db, agentviewID, 4)

	stats := env.engine.ResyncAll(context.Background(), nil)
	for _, w := range stats.Warnings {
		require.False(t, strings.Contains(w, "resync aborted"), "ResyncAll aborted for Kiro SQLite-only dataset: %s", w)
	}
	require.NotZero(t, stats.Synced, "expected Kiro SQLite sessions to be synced")
	assertSessionMessageCount(t, env.db, agentviewID, 4)
}

func TestResyncAllMixedOpenCodeRootsKeepsSQLiteFallback(t *testing.T) {
	storageBase := t.TempDir()
	storageRoot := filepath.Join(storageBase, "storage#root")
	sqliteRoot := t.TempDir()
	err := os.MkdirAll(filepath.Join(
		storageRoot, "storage", "session", "global",
	), 0o755)
	require.NoError(t, err, "mkdir storage root")

	env := setupTestEnv(
		t, WithOpenCodeDirs([]string{storageRoot, sqliteRoot}),
	)

	oc := createOpenCodeDB(t, sqliteRoot)
	oc.addProject(t, "proj-1", "/home/user/code/myapp")

	sessionID := "oc-resync-sqlite-fallback"
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
		"hello sqlite fallback", timeCreated,
	)
	oc.addTextPart(
		t, "part-a1", sessionID, "msg-a1",
		"hi sqlite fallback", timeCreated+1,
	)

	env.engine.SyncAll(context.Background(), nil)
	agentviewID := "opencode:" + sessionID
	assertSessionMessageCount(t, env.db, agentviewID, 2)

	stats := env.engine.ResyncAll(context.Background(), nil)

	for _, w := range stats.Warnings {
		require.False(t, strings.Contains(w, "resync aborted"), "ResyncAll aborted for mixed OpenCode roots: %s", w)
	}
	require.NotZero(t, stats.Synced, "expected SQLite fallback OpenCode session to be synced")

	assertSessionMessageCount(t, env.db, agentviewID, 2)
	assertMessageContent(
		t, env.db, agentviewID,
		"hello sqlite fallback",
		"hi sqlite fallback",
	)
}

func TestResyncAllOpenCodeStorageArchiveHandlesSQLiteFallbackFreshness(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	storage := createOpenCodeStorageFixture(t, env.opencodeDir)

	staleSessionID := "oc-storage-to-stale-sqlite"
	storage.addSession(
		t, "global", staleSessionID,
		"/home/user/code/myapp", "Storage Then Stale SQLite",
		1704067200000, 1704067205000,
	)
	storage.addMessage(
		t, staleSessionID, "msg-stale-u1", "user",
		1704067200000, nil,
	)
	storage.addTextPart(
		t, staleSessionID, "msg-stale-u1", "part-stale-u1",
		"hello stale storage", 1704067200000,
	)
	newerSessionID := "oc-storage-to-newer-sqlite"
	storage.addSession(
		t, "global", newerSessionID,
		"/home/user/code/myapp", "Storage Then Newer SQLite",
		1704067200000, 1704067205000,
	)
	storage.addMessage(
		t, newerSessionID, "msg-newer-u1", "user",
		1704067200000, nil,
	)
	storage.addTextPart(
		t, newerSessionID, "msg-newer-u1", "part-newer-u1",
		"hello newer storage", 1704067200000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2,
		Synced:        2,
		Skipped:       0,
	})

	err := os.RemoveAll(
		filepath.Join(env.opencodeDir, "storage"),
	)
	require.NoError(t, err, "remove storage tree")

	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj-1", "/home/user/code/myapp")
	oc.addSession(
		t, staleSessionID, "proj-1",
		1704067200000, 1704067209000,
	)
	oc.addMessage(
		t, "msg-stale-u1", staleSessionID, "user",
		1704067200000,
	)
	oc.addTextPart(
		t, "part-stale-u1", staleSessionID, "msg-stale-u1",
		"hello stale sqlite fallback", 1704067200000,
	)

	sqliteUpdatedAt := time.Now().Add(2 * time.Second).UnixMilli()
	oc.addSession(
		t, newerSessionID, "proj-1",
		1704067200000, sqliteUpdatedAt,
	)
	oc.addMessage(
		t, "msg-newer-u1", newerSessionID, "user",
		1704067200000,
	)
	oc.addTextPart(
		t, "part-newer-u1", newerSessionID, "msg-newer-u1",
		"hello newer sqlite fallback", 1704067200000,
	)

	stats := env.engine.ResyncAll(context.Background(), nil)
	for _, w := range stats.Warnings {
		require.False(t, strings.Contains(w, "resync aborted"), "ResyncAll aborted for storage->sqlite freshness: %s", w)
	}
	require.NotZero(t, stats.Synced, "expected newer sqlite fallback to be synced")

	assertMessageContent(
		t, env.db, "opencode:"+staleSessionID,
		"hello stale storage",
	)
	assertMessageContent(
		t, env.db, "opencode:"+newerSessionID,
		"hello newer sqlite fallback",
	)
}

func TestResyncAllKiloStorageArchivePreservesStaleSQLiteFallback(
	t *testing.T,
) {

	env := setupSingleAgentTestEnv(t, parser.AgentKilo)
	storage := createOpenCodeStorageFixture(t, env.kiloDir)

	sessionID := "kilo-storage-to-sqlite"
	storage.addSession(
		t, "global", sessionID,
		"/home/user/code/kilo-app", "Storage Then SQLite",
		1704067200000, 1704067205000,
	)
	storage.addMessage(
		t, sessionID, "msg-u1", "user",
		1704067200000, nil,
	)
	storage.addTextPart(
		t, sessionID, "msg-u1", "part-u1",
		"hello kilo storage", 1704067200000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	err := os.RemoveAll(
		filepath.Join(env.kiloDir, "storage"),
	)
	require.NoError(t, err, "remove storage tree")

	sqlite := createKiloDB(t, env.kiloDir)
	sqlite.addProject(t, "proj-1", "/home/user/code/kilo-app")
	sqlite.addSession(
		t, sessionID, "proj-1",
		1704067200000, 1704067209000,
	)
	sqlite.addMessage(
		t, "msg-u1", sessionID, "user",
		1704067200000,
	)
	sqlite.addTextPart(
		t, "part-u1", sessionID, "msg-u1",
		"hello kilo sqlite fallback", 1704067200000,
	)

	stats := env.engine.ResyncAll(context.Background(), nil)
	require.False(t, stats.Aborted, "ResyncAll aborted for Kilo storage archive")
	for _, w := range stats.Warnings {
		require.False(t, strings.Contains(w, "resync aborted"), "ResyncAll aborted for Kilo storage->sqlite fallback: %s", w)
	}
	require.Equal(t, 0, stats.Synced, "stats.Synced = %d, want 0", stats.Synced)

	assertMessageContent(
		t, env.db, "kilo:"+sessionID,
		"hello kilo storage",
	)

	sqliteOnlyID := "kilo-sqlite-only"
	sqlite.addSession(
		t, sqliteOnlyID, "proj-1",
		1704067210000, 1704067215000,
	)
	sqlite.addMessage(
		t, "sqlite-only-msg-a1", sqliteOnlyID, "assistant",
		1704067211000,
	)
	sqlite.addTextPart(
		t, "sqlite-only-part-a1", sqliteOnlyID, "sqlite-only-msg-a1",
		"sqlite-only kilo reply", 1704067211000,
	)

	stats = env.engine.ResyncAll(context.Background(), nil)
	require.False(t, stats.Aborted, "ResyncAll aborted for Kilo SQLite-only session")
	for _, w := range stats.Warnings {
		require.False(t, strings.Contains(w, "resync aborted"), "ResyncAll aborted for Kilo SQLite-only session: %s", w)
	}
	require.NotZero(t, stats.Synced, "expected Kilo SQLite-only session to be synced")
	assertMessageContent(
		t, env.db, "kilo:"+sqliteOnlyID,
		"sqlite-only kilo reply",
	)
	assertMessageContent(
		t, env.db, "kilo:"+sessionID,
		"hello kilo storage",
	)
}

func TestResyncAllOpenCodeStorageMissingMessagePreservesArchive(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeStorageFixture(t, env.opencodeDir)

	sessionID := "oc-resync-missing-message"
	sessionPath := oc.addSession(
		t, "global", sessionID,
		"/home/user/code/myapp", "Resync Missing Message",
		1704067200000, 1704067205000,
	)
	oc.addMessage(
		t, sessionID, "msg-u1", "user",
		1704067200000, nil,
	)
	oc.addTextPart(
		t, sessionID, "msg-u1", "part-u1",
		"question", 1704067200000,
	)
	messagePath := oc.addMessage(
		t, sessionID, "msg-a1", "assistant",
		1704067201000, nil,
	)
	oc.addTextPart(
		t, sessionID, "msg-a1", "part-a1",
		"answer", 1704067201000,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	require.NoError(t, os.Remove(messagePath), "remove message file")
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(sessionPath, future, future), "touch session path")

	stats := env.engine.ResyncAll(context.Background(), nil)
	for _, w := range stats.Warnings {
		require.False(t, strings.Contains(w, "resync aborted"), "ResyncAll aborted for missing OpenCode message: %s", w)
	}

	assertMessageContent(
		t, env.db, "opencode:"+sessionID,
		"question", "answer",
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
	env.engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, env.db, "mixed-file", 2)
	assertSessionMessageCount(
		t, env.db, "opencode:"+sessionID, 2,
	)

	// Remove all file-based sessions to simulate empty
	// file discovery. OpenCode data remains.
	entries, err := os.ReadDir(
		filepath.Join(env.claudeDir, "proj"),
	)
	require.NoError(t, err, "reading dir")
	for _, e := range entries {
		p := filepath.Join(env.claudeDir, "proj", e.Name())
		os.Remove(p)
	}

	stats := env.engine.ResyncAll(context.Background(), nil)

	// Must abort: file-backed sessions would be lost.
	hasAbortWarning := false
	for _, w := range stats.Warnings {
		if strings.Contains(w, "resync aborted") {
			hasAbortWarning = true
		}
	}
	assert.True(t, hasAbortWarning, "expected abort when file dirs are empty "+"but old DB has file-backed sessions")

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
	stats := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Synced, "Synced = %d, want 1 (engine used mutated map)", stats.Synced)
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

	stats2 := engine2.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats2.Synced, "Synced = %d, want 1 (engine used aliased slice)", stats2.Synced)
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
			assert.Equal(t, "amp", sess.Agent, "agent = %q, want amp", sess.Agent)
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
			assert.Equal(t, "amp", sess.Agent, "agent = %q, want amp", sess.Agent)
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
	require.NoError(t, err)
	assert.Equal(t, 0, len(page.Sessions), "expected 0 sessions (.json skipped), got %d", len(page.Sessions))
}

func TestSyncPathsVSCodeCopilotWorkspaceMetadataRefreshesProject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	dir := t.TempDir()
	vscDir := filepath.Join(dir, "vscode")
	hashDir := filepath.Join(vscDir, "workspaceStorage", "abc123")
	chatDir := filepath.Join(hashDir, "chatSessions")
	workspacePath := filepath.Join(hashDir, "workspace.json")

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCodeCopilot: {vscDir},
		},
		Machine: "local",
	})

	writeWorkspace := func(name string) {
		t.Helper()
		dbtest.WriteTestFile(t, workspacePath, fmt.Appendf(nil,
			`{"folder":"file:///Users/alice/code/%s"}`,
			name,
		))
	}

	uuid := "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
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
	jsonlPath := filepath.Join(chatDir, uuid+".jsonl")

	writeWorkspace("one")
	dbtest.WriteTestFile(
		t, jsonlPath,
		[]byte(`{"kind":0,"v":`+session+`}`),
	)

	engine.SyncPaths([]string{jsonlPath})
	assertSessionState(
		t, database, "vscode-copilot:"+uuid,
		func(sess *db.Session) {
			assert.Equal(t, "one", sess.Project)
		},
	)

	info, err := os.Stat(jsonlPath)
	require.NoError(t, err, "stat vscode copilot session")
	engine.InjectSkipCache(map[string]int64{
		jsonlPath: info.ModTime().UnixNano(),
	})

	writeWorkspace("two")
	engine.SyncPaths([]string{workspacePath})
	assertSessionState(
		t, database, "vscode-copilot:"+uuid,
		func(sess *db.Session) {
			assert.Equal(t, "two", sess.Project)
		},
	)

	writeWorkspace("three")
	engine.SyncPaths([]string{jsonlPath, workspacePath})
	assertSessionState(
		t, database, "vscode-copilot:"+uuid,
		func(sess *db.Session) {
			assert.Equal(t, "three", sess.Project)
		},
	)
}

func TestSyncPathsVSCodeCopilotPersistsUsageEvents(t *testing.T) {
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

	uuid := "cccccccc-dddd-eeee-ffff-000000000000"
	session := fmt.Sprintf(
		`{"version":3,"sessionId":"%s",`+
			`"creationDate":1704103200000,`+
			`"lastMessageDate":1704103260000,`+
			`"requests":[{"requestId":"r1",`+
			`"message":{"text":"hello"},`+
			`"response":[{"value":"hi"}],`+
			`"timestamp":1704103200000,`+
			`"modelId":"copilot/claude-opus-4.8",`+
			`"result":{"metadata":{`+
			`"promptTokens":12,`+
			`"outputTokens":3,`+
			`"resolvedModel":"claude-opus-4-8"}}}]}`,
		uuid,
	)
	jsonPath := filepath.Join(chatDir, uuid+".json")
	dbtest.WriteTestFile(t, jsonPath, []byte(session))

	engine.SyncPaths([]string{jsonPath})

	ctx := context.Background()
	sessionID := "vscode-copilot:" + uuid
	events, err := database.GetUsageEvents(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "vscode-copilot", events[0].Source)
	assert.Equal(t, "claude-opus-4-8", events[0].Model)
	assert.Equal(t, 12, events[0].InputTokens)
	assert.Equal(t, 3, events[0].OutputTokens)

	require.NoError(t, engine.SyncSingleSession(sessionID))
	events, err = database.GetUsageEvents(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "claude-opus-4-8", events[0].Model)
}

func TestSyncPathsPositronJSONLPriority(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	dir := t.TempDir()
	positronDir := filepath.Join(dir, "positron")
	chatDir := filepath.Join(
		positronDir, "workspaceStorage", "abc123",
		"chatSessions",
	)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPositron: {positronDir},
		},
		Machine: "local",
	})

	uuid := "cccccccc-dddd-eeee-ffff-aaaaaaaaaaaa"
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

	engine.SyncPaths([]string{jsonPath})

	page, err := database.ListSessions(
		context.Background(), db.SessionFilter{Limit: 10},
	)
	require.NoError(t, err)
	assert.Equal(t, 0, len(page.Sessions), "expected 0 sessions (.json skipped), got %d", len(page.Sessions))
}

func TestSyncAllPositronJSONLPriority(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	dir := t.TempDir()
	positronDir := filepath.Join(dir, "positron")
	hashDir := filepath.Join(positronDir, "workspaceStorage", "abc123")
	chatDir := filepath.Join(hashDir, "chatSessions")

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPositron: {positronDir},
		},
		Machine: "local",
	})

	uuid := "cccccccc-dddd-eeee-ffff-bbbbbbbbbbbb"
	jsonSession := fmt.Sprintf(
		`{"version":1,"sessionId":"%s",`+
			`"creationDate":1704103200000,`+
			`"lastMessageDate":1704103260000,`+
			`"requests":[{"requestId":"r1",`+
			`"message":{"text":"json fallback"},`+
			`"response":[{"value":"json response"}],`+
			`"timestamp":1704103200000}]}`,
		uuid,
	)
	jsonlSession := fmt.Sprintf(
		`{"version":1,"sessionId":"%s",`+
			`"creationDate":1704103200000,`+
			`"lastMessageDate":1704103260000,`+
			`"requests":[{"requestId":"r1",`+
			`"message":{"text":"jsonl preferred"},`+
			`"response":[{"value":"jsonl response"}],`+
			`"timestamp":1704103200000}]}`,
		uuid,
	)

	jsonPath := filepath.Join(chatDir, uuid+".json")
	jsonlPath := filepath.Join(chatDir, uuid+".jsonl")
	dbtest.WriteTestFile(t, jsonPath, []byte(jsonSession))
	dbtest.WriteTestFile(
		t, jsonlPath,
		[]byte(`{"kind":0,"v":`+jsonlSession+`}`),
	)

	stats := engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 1, stats.Synced, "synced = %d, want 1", stats.Synced)

	sess, err := database.GetSession(context.Background(), "positron:"+uuid)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assertSessionMessageCount(t, database, "positron:"+uuid, 2)
	msgs := fetchMessages(t, database, "positron:"+uuid)
	require.NotEmpty(t, msgs)
	assert.Equal(t, "jsonl preferred", msgs[0].Content)
}

func TestSyncPathsPositronWorkspaceMetadataRefreshesProject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	dir := t.TempDir()
	positronDir := filepath.Join(dir, "positron")
	hashDir := filepath.Join(positronDir, "workspaceStorage", "abc123")
	chatDir := filepath.Join(hashDir, "chatSessions")
	workspacePath := filepath.Join(hashDir, "workspace.json")

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPositron: {positronDir},
		},
		Machine: "local",
	})

	writeWorkspace := func(name string) {
		t.Helper()
		dbtest.WriteTestFile(t, workspacePath, fmt.Appendf(nil,
			`{"folder":"file:///Users/alice/code/%s"}`,
			name,
		))
	}

	uuid := "dddddddd-eeee-ffff-aaaa-bbbbbbbbbbbb"
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
	jsonlPath := filepath.Join(chatDir, uuid+".jsonl")

	writeWorkspace("one")
	dbtest.WriteTestFile(
		t, jsonlPath,
		[]byte(`{"kind":0,"v":`+session+`}`),
	)

	engine.SyncPaths([]string{jsonlPath})
	assertSessionState(
		t, database, "positron:"+uuid,
		func(sess *db.Session) {
			assert.Equal(t, "one", sess.Project)
		},
	)

	info, err := os.Stat(jsonlPath)
	require.NoError(t, err, "stat positron session")
	engine.InjectSkipCache(map[string]int64{
		jsonlPath: info.ModTime().UnixNano(),
	})

	writeWorkspace("two")
	engine.SyncPaths([]string{workspacePath})
	assertSessionState(
		t, database, "positron:"+uuid,
		func(sess *db.Session) {
			assert.Equal(t, "two", sess.Project)
		},
	)

	writeWorkspace("three")
	engine.SyncPaths([]string{jsonlPath, workspacePath})
	assertSessionState(
		t, database, "positron:"+uuid,
		func(sess *db.Session) {
			assert.Equal(t, "three", sess.Project)
		},
	)
}

func TestSyncAllSincePositronWorkspaceMetadataRefreshesProject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	dir := t.TempDir()
	positronDir := filepath.Join(dir, "positron")
	hashDir := filepath.Join(positronDir, "workspaceStorage", "abc123")
	chatDir := filepath.Join(hashDir, "chatSessions")
	workspacePath := filepath.Join(hashDir, "workspace.json")

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPositron: {positronDir},
		},
		Machine: "local",
	})

	writeWorkspace := func(name string) {
		t.Helper()
		dbtest.WriteTestFile(t, workspacePath, fmt.Appendf(nil,
			`{"folder":"file:///Users/alice/code/%s"}`,
			name,
		))
	}

	uuid := "dddddddd-eeee-ffff-aaaa-cccccccccccc"
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
	jsonlPath := filepath.Join(chatDir, uuid+".jsonl")

	writeWorkspace("one")
	dbtest.WriteTestFile(
		t, jsonlPath,
		[]byte(`{"kind":0,"v":`+session+`}`),
	)

	engine.SyncAll(context.Background(), nil)
	assertSessionState(
		t, database, "positron:"+uuid,
		func(sess *db.Session) {
			assert.Equal(t, "one", sess.Project)
		},
	)

	oldTime := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(jsonlPath, oldTime, oldTime), "chtimes session")
	require.NoError(t, os.Chtimes(workspacePath, oldTime, oldTime), "chtimes workspace")
	cutoff := time.Now().Add(-1 * time.Hour)

	writeWorkspace("two")
	stats := engine.SyncAllSince(context.Background(), cutoff, nil)
	assert.Equal(t, 1, stats.Synced, "synced = %d, want 1", stats.Synced)

	assertSessionState(
		t, database, "positron:"+uuid,
		func(sess *db.Session) {
			assert.Equal(t, "two", sess.Project)
		},
	)
}

func TestPiSessionIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Build a temp pi session directory:
	//   <piDir>/<encoded-cwd>/<session-id>.jsonl
	piDir := t.TempDir()
	cwdSubdir := filepath.Join(piDir, "--Users-alice-code-my-project")
	require.NoError(t, os.MkdirAll(cwdSubdir, 0o755))

	// Use the existing pi session fixture from the parser testdata.
	// The fixture has id="pi-test-session-uuid".
	_, callerFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(
		filepath.Dir(callerFile), "..", "parser", "testdata", "pi",
	)
	fixtureContent, err := os.ReadFile(
		filepath.Join(fixtureDir, "session.jsonl"),
	)
	require.NoError(t, err, "reading pi fixture")

	sessionFile := filepath.Join(cwdSubdir, "pi-test-session-uuid.jsonl")
	dbtest.WriteTestFile(t, sessionFile, fixtureContent)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPi: {piDir},
		},
		Machine: "local",
	})

	stats := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Synced, "expected 1 synced session, got %d (failed=%d)", stats.Synced, stats.Failed)

	assertSessionState(t, database, "pi:pi-test-session-uuid",
		func(sess *db.Session) {
			assert.Equal(t, "pi", sess.Agent, "agent = %q, want %q", sess.Agent, "pi")
			// The fixture has 2 real user messages. model_change and
			// compaction entries must not inflate the count after
			// postFilterCounts re-counts role="user" messages.
			assert.Equal(t, 2, sess.UserMessageCount, "UserMessageCount = %d, want 2", sess.UserMessageCount)
		},
	)

	// FindSourceFile should locate pi sessions via the "pi:" prefix.
	src := engine.FindSourceFile("pi:pi-test-session-uuid")
	assert.NotEmpty(t, src, "FindSourceFile returned empty for pi session")

	// SyncSingleSession should work for pi sessions.
	require.NoError(t, engine.SyncSingleSession("pi:pi-test-session-uuid"), "SyncSingleSession pi")
	assertSessionState(t, database, "pi:pi-test-session-uuid",
		func(sess *db.Session) {
			assert.Equal(t, "pi", sess.Agent, "after SyncSingleSession: agent = %q, want %q", sess.Agent, "pi")
		},
	)
}

func TestOMPSyncAllAndChangedPathUseProvider(t *testing.T) {
	env := setupTestEnv(t)
	path := env.writeSession(
		t,
		env.ompDir,
		filepath.Join("encoded-cwd", "omp-sync.jsonl"),
		piLikeProviderFixture("omp-sync", "/Users/alice/code/omp-app"),
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})
	assertSessionState(t, env.db, "omp:omp-sync", func(sess *db.Session) {
		assert.Equal(t, "omp", sess.Agent)
		assert.Equal(t, "omp_app", sess.Project)
	})
	assert.Equal(t, path, env.engine.FindSourceFile("omp:omp-sync"))

	updated := piLikeProviderFixture("omp-sync", "/Users/alice/code/omp-renamed")
	dbtest.WriteTestFile(t, path, []byte(updated))
	env.engine.SyncPaths([]string{path})

	assertSessionState(t, env.db, "omp:omp-sync", func(sess *db.Session) {
		assert.Equal(t, "omp", sess.Agent)
		assert.Equal(t, "omp_renamed", sess.Project)
	})
}

func piLikeProviderFixture(sessionID, cwd string) string {
	return strings.Join([]string{
		`{"type":"session","version":3,"id":"` + sessionID + `","timestamp":"2025-01-01T10:00:00Z","cwd":"` + cwd + `"}`,
		`{"type":"message","id":"msg-1","timestamp":"2025-01-01T10:00:01Z","message":{"role":"user","content":"Inspect the source."}}`,
		`{"type":"message","id":"msg-2","timestamp":"2025-01-01T10:00:02Z","message":{"role":"assistant","content":"Ready.","model":"claude-opus-4-5"}}`,
	}, "\n")
}

func TestIncrementalSync_ClaudeAppend(t *testing.T) {
	env := setupTestEnv(t)

	// Initial sync: one user message.
	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", tsZero),
	)
	path := env.writeClaudeSession(
		t, "proj", "inc-test.jsonl", initial,
	)
	env.engine.SyncAll(context.Background(), nil)

	assertSessionMessageCount(t, env.db, "inc-test", 1)
	assertMessageRoles(t, env.db, "inc-test", "user")
	msgs := fetchMessages(t, env.db, "inc-test")
	require.Equal(t, "inc-test", msgs[0].SessionID, "msgs[0].SessionID = %q, want inc-test", msgs[0].SessionID)

	// Verify metadata is set from full parse.
	full, err := env.db.GetSessionFull(
		context.Background(), "inc-test",
	)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, full.FileHash, "file_hash not set after full parse")
	require.NotEmpty(t, *full.FileHash, "file_hash not set after full parse")
	origHash := *full.FileHash

	// Append an assistant response.
	appendedJSON, err := json.Marshal(map[string]any{
		"type":      "assistant",
		"timestamp": tsZeroS5,
		"message": map[string]any{
			"model": "claude-sonnet-4-20250514",
			"usage": map[string]any{
				"input_tokens":                100,
				"cache_creation_input_tokens": 200,
				"cache_read_input_tokens":     200,
				"output_tokens":               200,
			},
			"content": []map[string]any{
				{"type": "text", "text": "world"},
			},
		},
	})
	require.NoError(t, err, "marshal assistant fixture")
	appended := string(appendedJSON) + "\n"
	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	f.Close()
	require.NoError(t, err, "append")

	// SyncPaths triggers incremental parse.
	env.engine.SyncPaths([]string{path})

	// Session count updated.
	assertSessionMessageCount(t, env.db, "inc-test", 2)
	assertMessageRoles(
		t, env.db, "inc-test", "user", "assistant",
	)

	// New message has correct session_id.
	msgs = fetchMessages(t, env.db, "inc-test")
	for i, m := range msgs {
		assert.Equal(t, "inc-test", m.SessionID, "msgs[%d].SessionID = %q, want inc-test", i, m.SessionID)
	}

	// Metadata preserved (file_hash not cleared).
	updated, err := env.db.GetSessionFull(
		context.Background(), "inc-test",
	)
	require.NoError(t, err, "GetSessionFull after incremental")
	require.NotNil(t, updated.FileHash, "file_hash = nil, want %q (preserved)", origHash)
	assert.Equal(t, origHash, *updated.FileHash,
		"file_hash = %v, want %q (preserved)", updated.FileHash, origHash)
	assert.True(t, updated.HasTotalOutputTokens, "HasTotalOutputTokens = false, want true")
	assert.True(t, updated.HasPeakContextTokens, "HasPeakContextTokens = false, want true")
	assert.Equal(t, 200, updated.TotalOutputTokens, "TotalOutputTokens = %d, want 200", updated.TotalOutputTokens)
	assert.Equal(t, 500, updated.PeakContextTokens, "PeakContextTokens = %d, want 500", updated.PeakContextTokens)
	assert.True(t, msgs[1].HasContextTokens, "assistant HasContextTokens = false, want true")
	assert.True(t, msgs[1].HasOutputTokens, "assistant HasOutputTokens = false, want true")
	assert.Equal(t, 200, msgs[1].OutputTokens, "assistant OutputTokens = %d, want 200", msgs[1].OutputTokens)
	assert.Equal(t, 500, msgs[1].ContextTokens, "assistant ContextTokens = %d, want 500", msgs[1].ContextTokens)
}

func TestIncrementalSync_ClaudeFilteredTailAdvancesNextOrdinal(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	initial := testjsonl.JoinJSONL(
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"go"},"cwd":"/tmp"}`,
	)
	path := env.writeClaudeSession(
		t, "proj", "filtered-tail.jsonl", initial,
	)
	env.engine.SyncAll(context.Background(), nil)

	firstAppend := testjsonl.JoinJSONL(
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_pair","name":"Read","input":{"file_path":"README.md"}}],"usage":{"input_tokens":1,"output_tokens":1},"stop_reason":"tool_use"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:02Z","uuid":"r1","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_pair","content":"ok"}]}}`,
	) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open first append")
	_, err = f.WriteString(firstAppend)
	require.NoError(t, err, "write first append")
	require.NoError(t, f.Close())

	env.engine.SyncPaths([]string{path})

	secondAppend := testjsonl.JoinJSONL(
		`{"type":"assistant","timestamp":"2024-01-01T10:00:03Z","uuid":"a2","parentUuid":"r1","message":{"content":[{"type":"text","text":"done"}],"usage":{"input_tokens":1,"output_tokens":2},"stop_reason":"end_turn"}}`,
	) + "\n"
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open second append")
	_, err = f.WriteString(secondAppend)
	require.NoError(t, err, "write second append")
	require.NoError(t, f.Close())

	env.engine.SyncPaths([]string{path})

	msgs := fetchMessages(t, env.db, "filtered-tail")
	require.Len(t, msgs, 3)
	assert.Equal(t, 0, msgs[0].Ordinal, "ordinal 0")
	assert.Equal(t, 1, msgs[1].Ordinal, "ordinal 1")
	assert.Equal(t, 3, msgs[2].Ordinal, "ordinal 2")
}

func TestIncrementalSync_ClaudeQueueOperationPreservesSubagentMapping(t *testing.T) {
	env := setupTestEnv(t)

	initial := testjsonl.JoinJSONL(
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"go"},"cwd":"/tmp"}`,
	)
	path := env.writeClaudeSession(
		t, "proj", "queued-subagent.jsonl", initial,
	)
	env.engine.SyncAll(context.Background(), nil)

	appended := testjsonl.JoinJSONL(
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_queue","name":"Agent","input":{"description":"inspect","subagent_type":"Explore","prompt":"inspect"}}],"usage":{"input_tokens":1,"output_tokens":1},"stop_reason":"tool_use"}}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2024-01-01T10:00:02Z","sessionId":"queued-subagent","content":"{\"task_id\":\"childqueue\",\"tool_use_id\":\"toolu_queue\",\"description\":\"inspect\",\"task_type\":\"local_agent\"}"}`,
	) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	require.NoError(t, err, "append queue mapping")
	require.NoError(t, f.Close())

	env.engine.SyncPaths([]string{path})

	msgs := fetchMessages(t, env.db, "queued-subagent")
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(
		t,
		"agent-childqueue",
		msgs[1].ToolCalls[0].SubagentSessionID,
		"subagent_session_id",
	)
}

func TestIncrementalSync_ClaudeQueueOperationOnlyRepairsStoredSubagentMapping(
	t *testing.T,
) {
	env := setupTestEnv(t)

	initial := testjsonl.JoinJSONL(
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"go"},"cwd":"/tmp"}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_queue_only","name":"Agent","input":{"description":"inspect","subagent_type":"Explore","prompt":"inspect"}}],"usage":{"input_tokens":1,"output_tokens":1},"stop_reason":"tool_use"}}`,
	)
	path := env.writeClaudeSession(
		t, "proj", "queued-subagent-split.jsonl", initial,
	)
	env.engine.SyncAll(context.Background(), nil)

	appended := testjsonl.JoinJSONL(
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2024-01-01T10:00:02Z","sessionId":"queued-subagent-split","content":"{\"task_id\":\"childqueueonly\",\"tool_use_id\":\"toolu_queue_only\",\"description\":\"inspect\",\"task_type\":\"local_agent\"}"}`,
	) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	require.NoError(t, err, "append queue mapping")
	require.NoError(t, f.Close())

	env.engine.SyncPaths([]string{path})

	msgs := fetchMessages(t, env.db, "queued-subagent-split")
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(
		t,
		"agent-childqueueonly",
		msgs[1].ToolCalls[0].SubagentSessionID,
		"subagent_session_id",
	)
}

func TestIncrementalSync_ClaudeProgressOnlyRepairsStoredSubagentMapping(
	t *testing.T,
) {
	env := setupTestEnv(t)

	initial := testjsonl.JoinJSONL(
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"go"},"cwd":"/tmp"}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_progress_only","name":"Agent","input":{"description":"inspect","subagent_type":"Explore","prompt":"inspect"}}],"usage":{"input_tokens":1,"output_tokens":1},"stop_reason":"tool_use"}}`,
	)
	path := env.writeClaudeSession(
		t, "proj", "progress-subagent-split.jsonl", initial,
	)
	env.engine.SyncAll(context.Background(), nil)

	appended := testjsonl.JoinJSONL(
		`{"type":"progress","timestamp":"2024-01-01T10:00:02Z","parentToolUseID":"toolu_progress_only","data":{"type":"agent_progress","agentId":"childprogressonly"}}`,
	) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	require.NoError(t, err, "append progress mapping")
	require.NoError(t, f.Close())

	env.engine.SyncPaths([]string{path})

	msgs := fetchMessages(t, env.db, "progress-subagent-split")
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(
		t,
		"agent-childprogressonly",
		msgs[1].ToolCalls[0].SubagentSessionID,
		"subagent_session_id",
	)
}

// TestIncrementalSync_ClaudeFileReplaced verifies that when a
// session file is replaced atomically (new inode/device), the
// sync engine detects the identity change and falls back to a
// full parse instead of treating the new content as an append.
func TestIncrementalSync_ClaudeFileReplaced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("identity tracking is a no-op on Windows")
	}
	env := setupTestEnv(t)

	original := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("first", tsZero),
	)
	path := env.writeClaudeSession(
		t, "proj", "replaced.jsonl", original,
	)
	env.engine.SyncAll(context.Background(), nil)

	assertSessionMessageCount(t, env.db, "replaced", 1)

	full, err := env.db.GetSessionFull(
		context.Background(), "replaced",
	)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, full.FileInode, "file_inode not populated after initial sync")
	require.NotZero(t, *full.FileInode, "file_inode not populated after initial sync")
	origInode := *full.FileInode

	// Atomically replace the file. The content is longer than the
	// original so an incremental parse would mistakenly append the
	// new file's bytes past the old offset.
	replacement := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("second", tsZero),
		testjsonl.ClaudeUserJSON("third", tsZeroS5),
	)
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, []byte(replacement), 0o644), "write replacement")
	require.NoError(t, os.Rename(tmp, path), "rename replacement")

	env.engine.SyncPaths([]string{path})

	// The stored inode must track the new file (i.e. a full
	// parse re-ran and overwrote the identity). If the incremental
	// path had run instead, the old inode would still be stored
	// and the appended bytes would be interpreted as continuation
	// of the original file.
	full, err = env.db.GetSessionFull(
		context.Background(), "replaced",
	)
	require.NoError(t, err, "GetSessionFull after replace")
	require.NotNil(t, full.FileInode, "file_inode cleared after replace")
	assert.NotEqual(t, origInode, *full.FileInode, "file_inode = %d, want change from original", *full.FileInode)
	// File size in the DB should match the replacement, not the
	// pre-replacement size that an incremental parse would have
	// left in place.
	newInfo, err := os.Stat(path)
	require.NoError(t, err, "stat replacement")
	require.NotNil(t, full.FileSize, "file_size = nil, want %d (full-parse size)", newInfo.Size())
	assert.Equal(t, newInfo.Size(), *full.FileSize, "file_size = %v, want %d (full-parse size)", *full.FileSize, newInfo.Size())
}

func TestIncrementalSync_ClaudeTruncatedFileReplacesStoredMessages(t *testing.T) {
	env := setupTestEnv(t)

	original := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("first", tsZero),
		testjsonl.ClaudeAssistantJSON("stale assistant", tsZeroS5),
	)
	path := env.writeClaudeSession(
		t, "proj", "truncated-replace.jsonl", original,
	)
	env.engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, env.db, "truncated-replace", 2)

	replacement := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("replacement", tsZero),
	)
	require.Less(t, len(replacement), len(original), "replacement must truncate file")
	require.NoError(t, os.WriteFile(path, []byte(replacement), 0o644), "write truncated replacement")

	env.engine.SyncPaths([]string{path})

	assertSessionMessageCount(t, env.db, "truncated-replace", 1)
	msgs := fetchMessages(t, env.db, "truncated-replace")
	require.Len(t, msgs, 1)
	assert.Equal(t, "replacement", msgs[0].Content)
}

func TestIncrementalSync_ClaudeSameSizeFileReplaceUsesFullParse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("identity tracking is a no-op on Windows")
	}
	env := setupTestEnv(t)

	original := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("first", tsZero),
		testjsonl.ClaudeAssistantJSON("alpha", tsZeroS5),
	)
	path := env.writeClaudeSession(
		t, "proj", "same-size-replace.jsonl", original,
	)
	env.engine.SyncAll(context.Background(), nil)

	replacement := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("third", tsZero),
		testjsonl.ClaudeAssistantJSON("bravo", tsZeroS5),
	)
	require.Len(t, replacement, len(original), "replacement fixture must keep same byte size")
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, []byte(replacement), 0o644), "write replacement")
	require.NoError(t, os.Rename(tmp, path), "rename replacement")

	env.engine.SyncPaths([]string{path})

	msgs := fetchMessages(t, env.db, "same-size-replace")
	require.Len(t, msgs, 2)
	assert.Equal(t, "third", msgs[0].Content)
	assert.Equal(t, "bravo", msgs[1].Content)
}

func TestIncrementalSync_ClaudeSameSizeInPlaceRewriteClearsStaleRows(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	original := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("first", tsZero),
		testjsonl.ClaudeAssistantJSON("stale assistant", tsZeroS5),
	)
	path := env.writeClaudeSession(
		t, "proj", "same-size-in-place.jsonl", original,
	)
	env.engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, env.db, "same-size-in-place", 2)

	replacement := ""
	for padding := range 4096 {
		candidate := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON(
				"replacement"+strings.Repeat("x", padding),
				tsZero,
			),
		)
		if len(candidate) == len(original) {
			replacement = candidate
			break
		}
	}
	require.NotEmpty(t, replacement, "failed to build same-size replacement fixture")
	require.Len(t, replacement, len(original), "replacement fixture must keep same byte size")

	require.NoError(t, os.WriteFile(path, []byte(replacement), 0o644), "write in-place replacement")
	now := time.Now().Add(time.Second)
	require.NoError(t, os.Chtimes(path, now, now), "bump replacement mtime")

	env.engine.SyncPaths([]string{path})

	assertSessionMessageCount(t, env.db, "same-size-in-place", 1)
	msgs := fetchMessages(t, env.db, "same-size-in-place")
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].Content, "replacement")
}

// TestIncrementalSync_ClaudeMidStreamSplitFallsBackToFullParse covers
// the cross-sync split case: the first sync stores a partial assistant
// snapshot (one of several streaming snapshots) and the next sync
// appends a later snapshot of the SAME response (same message.id).
// The engine must detect the shared id and fall back to a full parse
// so the chunk merge collapses both snapshots into one assistant
// message instead of two.
func TestIncrementalSync_ClaudeMidStreamSplitFallsBackToFullParse(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	first, err := json.Marshal(map[string]any{
		"type":      "assistant",
		"timestamp": tsZeroS5,
		"uuid":      "a1",
		"message": map[string]any{
			"id":    "msg_split",
			"model": "claude-sonnet-4-20250514",
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 1,
			},
			"content": []map[string]any{
				{"type": "text", "text": "Hello"},
			},
			"stop_reason": "tool_use",
		},
	})
	require.NoError(t, err, "marshal first snapshot")

	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", tsZero),
		string(first),
	)
	path := env.writeClaudeSession(
		t, "proj", "split-stream.jsonl", initial,
	)
	env.engine.SyncAll(context.Background(), nil)

	assertSessionMessageCount(t, env.db, "split-stream", 2)

	// Append a continuation snapshot with the same message.id —
	// this is the second half of the same streaming response.
	second, err := json.Marshal(map[string]any{
		"type":       "assistant",
		"timestamp":  tsEarly,
		"uuid":       "a2",
		"parentUuid": "a1",
		"message": map[string]any{
			"id":    "msg_split",
			"model": "claude-sonnet-4-20250514",
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 2,
			},
			"content": []map[string]any{
				{"type": "text", "text": "Hello world"},
			},
			"stop_reason": "end_turn",
		},
	})
	require.NoError(t, err, "marshal second snapshot")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, writeErr := f.WriteString(string(second) + "\n")
	f.Close()
	require.NoError(t, writeErr, "append")

	env.engine.SyncPaths([]string{path})

	// After the full-parse fallback, the two same-message.id
	// snapshots are merged into ONE assistant message — total
	// message count stays at 2 (user + merged assistant).
	assertSessionMessageCount(t, env.db, "split-stream", 2)

	msgs := fetchMessages(t, env.db, "split-stream")
	require.Equal(t, 2, len(msgs), "len(msgs) = %d, want 2", len(msgs))
	require.Equal(t, "assistant", string(msgs[1].Role), "msgs[1].Role = %q, want assistant", msgs[1].Role)
	// The partial snapshot ("Hello") must be REPLACED by the final
	// snapshot ("Hello world"), not concatenated as additive content.
	assert.Equal(t, "Hello world", msgs[1].Content, "msgs[1].Content = %q, want exactly %q", msgs[1].Content, "Hello world")
}

// TestIncrementalSync_ClaudeAgentIDFallbackUpdatesStoredToolCall covers
// the cross-sync subagent linkage case: the first sync stores an
// assistant tool_use row with no subagent_session_id, and a later sync
// appends a tool_result whose toolUseResult.agentId should populate the
// already-stored tool_call. The parser signals
// IsIncrementalFullParseFallback, so the full-parse fallback must run
// with forceReplace=true; otherwise the append-only write path skips
// the existing row and the linkage is silently dropped.
func TestIncrementalSync_ClaudeAgentIDFallbackUpdatesStoredToolCall(t *testing.T) {
	env := setupTestEnv(t)

	parentInitial := testjsonl.JoinJSONL(
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"go"},"cwd":"/tmp"}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"u1","message":{"id":"msg_one","content":[{"type":"tool_use","id":"toolu_late","name":"Agent","input":{"description":"d","subagent_type":"Explore","prompt":"p"}}],"usage":{"input_tokens":1,"output_tokens":1},"stop_reason":"tool_use"}}`,
	)

	path := env.writeClaudeSession(
		t, "proj-late-link", "parent-late-link.jsonl", parentInitial,
	)

	subContent := testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID(
			tsEarly, "do thing", "parent-late-link",
		).
		AddClaudeAssistant(tsEarlyS5, "done").
		String()
	env.writeSession(
		t, env.claudeDir,
		filepath.Join(
			"proj-late-link", "parent-late-link",
			"subagents", "agent-childlate.jsonl",
		),
		subContent,
	)

	env.engine.SyncAll(context.Background(), nil)

	// Linkage starts empty (the toolUseResult hasn't appeared yet).
	var got sql.NullString
	require.NoError(t, env.db.Reader().QueryRow(`
		SELECT subagent_session_id
		FROM tool_calls
		WHERE session_id = ? AND tool_use_id = ?`,
		"parent-late-link", "toolu_late",
	).Scan(&got), "query before append")
	if got.Valid {
		require.Equal(t, "", got.String, "subagent_session_id = %q before tool_result, want empty", got.String)
	}

	// Append a tool_result with toolUseResult.agentId pointing at
	// the existing subagent session. Incremental parse will return
	// ErrClaudeIncrementalNeedsFullParse so the engine must full-
	// parse with forceReplace to update the stored tool_call row.
	toolResult := `{"type":"user","timestamp":"2024-01-01T10:00:02Z","uuid":"r1","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_late","content":"done"}]},"toolUseResult":{"status":"completed","agentId":"childlate"}}`
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, writeErr := f.WriteString(toolResult + "\n")
	f.Close()
	require.NoError(t, writeErr, "append")

	env.engine.SyncPaths([]string{path})

	require.NoError(t, env.db.Reader().QueryRow(`
		SELECT subagent_session_id
		FROM tool_calls
		WHERE session_id = ? AND tool_use_id = ?`,
		"parent-late-link", "toolu_late",
	).Scan(&got), "query after append")
	assert.Equal(t, "agent-childlate", got.String, "subagent_session_id = %q, want %q", got.String, "agent-childlate")
}

func TestIncrementalSync_ClaudeCrossSyncToolResultFallback(t *testing.T) {
	env := setupTestEnv(t)

	initial := testjsonl.JoinJSONL(
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"go"},"cwd":"/tmp"}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"u1","message":{"id":"msg_tool","content":[{"type":"tool_use","id":"toolu_cross","name":"Read","input":{"file_path":"README.md"}}],"usage":{"input_tokens":1,"output_tokens":1},"stop_reason":"tool_use"}}`,
	)
	path := env.writeClaudeSession(
		t, "proj-cross-tool", "cross-tool.jsonl", initial,
	)
	env.engine.SyncAll(context.Background(), nil)

	appended := `{"type":"user","timestamp":"2024-01-01T10:00:02Z","uuid":"r1","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_cross","content":"done"}]}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	require.NoError(t, err, "append tool_result")
	require.NoError(t, f.Close())

	env.engine.SyncPaths([]string{path})

	msgs := fetchMessages(t, env.db, "cross-tool")
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "done", msgs[1].ToolCalls[0].ResultContent, "result_content")
}

func TestIncrementalSync_CodexAppend(t *testing.T) {
	env := setupTestEnv(t)

	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"inc-cx", "/tmp/proj",
			"codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
	)
	path := env.writeCodexSession(
		t, filepath.Join("2024", "01", "01"),
		"rollout-20240101-inc-cx.jsonl", initial,
	)
	env.engine.SyncAll(context.Background(), nil)

	assertSessionMessageCount(
		t, env.db, "codex:inc-cx", 1,
	)

	// Append new messages.
	appended := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON(
			"assistant", "world", tsEarlyS5,
		),
	)
	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	f.Close()
	require.NoError(t, err, "append")

	env.engine.SyncPaths([]string{path})

	assertSessionMessageCount(
		t, env.db, "codex:inc-cx", 2,
	)
	assertMessageRoles(
		t, env.db, "codex:inc-cx", "user", "assistant",
	)

	// Verify session_id on all messages.
	msgs := fetchMessages(t, env.db, "codex:inc-cx")
	for i, m := range msgs {
		assert.Equal(t, "codex:inc-cx", m.SessionID, "msgs[%d].SessionID = %q, want codex:inc-cx", i, m.SessionID)
	}
}

// TestIncrementalSync_CodexStoresEffectiveMtime pins that the incremental
// append path persists the same session_index.jsonl-folded effective mtime a
// full Codex parse stores (parser.CodexEffectiveMtime). Without it an
// incrementally-synced Codex session would carry the plain rollout mtime,
// leaving the stored file_mtime on a different basis than the effective
// File.Mtime parse-diff's raced guard reads -- which would let an index newer
// than the rollout mask genuine transcript drift as raced.
func TestIncrementalSync_CodexStoresEffectiveMtime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	codexDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	env := setupTestEnv(t, WithCodexDirs([]string{codexDir}))

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c1229e1"
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/tmp/proj", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
	)
	path := env.writeCodexSession(
		t, filepath.Join("2024", "01", "01"),
		"rollout-2024-01-01T10-00-00-"+uuid+".jsonl", initial,
	)
	indexPath := filepath.Join(root, "session_index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"id":"`+uuid+`","thread_name":"Title",`+
			`"updated_at":"2024-01-01T10:00:00Z"}`+"\n",
	), 0o644))
	base := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(path, base, base), "chtimes initial session")
	require.NoError(t, os.Chtimes(indexPath, base, base), "chtimes initial index")
	env.engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, env.db, "codex:"+uuid, 1)

	// Append an assistant message so the next sync takes the incremental
	// append path (the title is unchanged, so no index-rename full parse).
	appended := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("assistant", "world", tsEarlyS5),
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	require.NoError(t, f.Close())
	require.NoError(t, err, "append")

	// Push the index strictly past the rollout so the effective mtime differs
	// from the plain rollout mtime.
	rollTime := time.Now().Add(-30 * time.Minute)
	require.NoError(t, os.Chtimes(path, rollTime, rollTime), "chtimes rollout")
	require.NoError(t, os.Chtimes(
		indexPath, rollTime.Add(time.Hour), rollTime.Add(time.Hour),
	), "chtimes index")

	env.engine.SyncPaths([]string{path})

	assertSessionMessageCount(t, env.db, "codex:"+uuid, 2)
	sess, err := env.db.GetSessionFull(context.Background(), "codex:"+uuid)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, sess, "session present")
	require.NotNil(t, sess.FileMtime, "file_mtime stored")

	// Compare against re-stats (not the requested Chtimes values) so the
	// assertion is robust to filesystem mtime granularity.
	idxInfo, err := os.Stat(indexPath)
	require.NoError(t, err, "stat index")
	rollInfo, err := os.Stat(path)
	require.NoError(t, err, "stat rollout")
	assert.Equal(t, idxInfo.ModTime().UnixNano(), *sess.FileMtime,
		"incremental Codex write stores the index-folded effective mtime")
	assert.Greater(t, *sess.FileMtime, rollInfo.ModTime().UnixNano(),
		"effective mtime exceeds the plain rollout mtime")
}

func TestIncrementalSync_CodexAppendFullReparseStoresRawFileSize(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupTestEnv(t)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c1229e4"
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/tmp/proj", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
	)
	path := env.writeCodexSession(
		t, filepath.Join("2024", "01", "01"),
		"rollout-2024-01-01T10-00-00-"+uuid+".jsonl", initial,
	)
	env.engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, env.db, "codex:"+uuid, 1)

	appended := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("assistant", "world", tsEarlyS5),
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	require.NoError(t, err, "append complete message")
	_, err = f.WriteString(`{"timestamp":"2024-01-01T10:00:10Z"`)
	require.NoError(t, err, "append partial trailing JSON")
	require.NoError(t, f.Close(), "close after append")

	env.engine.SyncPaths([]string{path})
	assertSessionMessageCount(t, env.db, "codex:"+uuid, 2)

	sess, err := env.db.GetSessionFull(context.Background(), "codex:"+uuid)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, sess, "session present")
	require.NotNil(t, sess.FileSize, "file_size stored")
	require.NotNil(t, sess.FileHash, "file_hash stored")

	live, err := os.ReadFile(path)
	require.NoError(t, err, "read live transcript")
	// Codex does not advertise incremental append, so re-syncing the appended
	// transcript is a full re-parse that stores the raw file size and hash
	// (including the ignored partial trailing line). The parsed-snapshot vs
	// partial-tail distinction is enforced at parse-diff time via
	// CodexTranscriptConsumedSize, not in the stored fingerprint.
	require.Equal(t, int64(len(live)), *sess.FileSize,
		"full Codex re-parse stores the raw file size")
	sum := sha256.Sum256(live)
	wantHash := fmt.Sprintf("%x", sum[:])
	assert.Equal(t, wantHash, *sess.FileHash,
		"stored Codex hash matches the whole-file fingerprint")
}

func TestIncrementalSync_CodexExecAppendRetainsEvents(t *testing.T) {
	env := setupTestEnv(t)

	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"inc-cx-exec", "/tmp/proj",
			"codex_exec", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "run command", tsEarlyS1),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_cmd",
			map[string]any{"cmd": "sleep 1"}, tsEarlyS5,
		),
	)
	path := env.writeCodexSession(
		t, filepath.Join("2024", "01", "01"),
		"rollout-20240101-inc-cx-exec.jsonl", initial,
	)
	env.engine.SyncAll(context.Background(), nil)

	appended := testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallOutputJSON(
			"call_cmd", "done", "2024-01-01T10:01:00Z",
		),
	)
	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	require.NoError(t, err, "append")
	require.NoError(t, f.Close())

	env.engine.SyncPaths([]string{path})

	msgs := fetchMessages(t, env.db, "codex:inc-cx-exec")
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "exec_command", msgs[1].ToolCalls[0].ToolName, "tool name")
	assert.Equal(t, "done", msgs[1].ToolCalls[0].ResultContent, "result_content")
}

func TestIncrementalSync_CodexLateTokenCountRewritesStoredMessage(t *testing.T) {
	env := setupTestEnv(t)

	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"inc-cx-late-usage", "/tmp/proj",
			"codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextJSON("gpt-5.5", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "run command", tsEarlyS1),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_cmd",
			map[string]any{"cmd": "sleep 1"}, tsEarlyS5,
		),
	)
	path := env.writeCodexSession(
		t, filepath.Join("2024", "01", "01"),
		"rollout-20240101-inc-cx-late-usage.jsonl",
		initial,
	)
	env.engine.SyncAll(context.Background(), nil)

	msgs := fetchMessages(t, env.db, "codex:inc-cx-late-usage")
	require.Len(t, msgs, 2)
	assert.Empty(t, msgs[1].TokenUsage)

	appended := testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallOutputJSON(
			"call_cmd", "done", "2024-01-01T10:01:00Z",
		),
		testjsonl.CodexTokenCountJSON(
			"2024-01-01T10:01:00Z", 100_000, 250, 64_000,
		),
	)
	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	require.NoError(t, err, "append")
	require.NoError(t, f.Close())

	env.engine.SyncPaths([]string{path})

	msgs = fetchMessages(t, env.db, "codex:inc-cx-late-usage")
	require.Len(t, msgs, 2)
	assert.NotEmpty(t, msgs[1].TokenUsage)
	assert.Equal(t, 250, msgs[1].OutputTokens)
	assert.Equal(t, 100_000, msgs[1].ContextTokens)
}

func TestIncrementalSync_CodexLateTokenCountWithIndexRenameRewritesStoredMessage(t *testing.T) {
	root := t.TempDir()
	codexDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	env := setupTestEnv(t, WithCodexDirs([]string{codexDir}))

	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e1"
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/tmp/proj",
			"codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextJSON("gpt-5.5", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "run command", tsEarlyS1),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_cmd",
			map[string]any{"cmd": "sleep 1"}, tsEarlyS5,
		),
	)
	path := env.writeCodexSession(
		t, filepath.Join("2024", "01", "01"),
		"rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
		initial,
	)

	indexPath := filepath.Join(root, "session_index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Original title","updated_at":"2024-01-01T10:00:00Z"}`+"\n",
		uuid,
	), 0o644))

	initialTime := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(path, initialTime, initialTime), "chtimes initial session")
	require.NoError(t, os.Chtimes(indexPath, initialTime, initialTime), "chtimes initial index")

	env.engine.SyncAll(context.Background(), nil)

	msgs := fetchMessages(t, env.db, "codex:"+uuid)
	require.Len(t, msgs, 2)
	assert.Empty(t, msgs[1].TokenUsage)

	appended := testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallOutputJSON(
			"call_cmd", "done", "2024-01-01T10:01:00Z",
		),
		testjsonl.CodexTokenCountJSON(
			"2024-01-01T10:01:00Z", 100_000, 250, 64_000,
		),
	)
	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	require.NoError(t, err, "append")
	require.NoError(t, f.Close())

	newTime := time.Now().Add(-30 * time.Minute)
	require.NoError(t, os.Chtimes(path, newTime, newTime), "chtimes appended session")
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Renamed title","updated_at":"2024-01-01T10:01:00Z"}`+"\n",
		uuid,
	), 0o644))
	require.NoError(t, os.Chtimes(indexPath, newTime.Add(time.Second), newTime.Add(time.Second)), "chtimes renamed index")

	env.engine.SyncPaths([]string{path})

	sess, err := env.db.GetSessionFull(context.Background(), "codex:"+uuid)
	require.NoError(t, err, "GetSessionFull after rename")
	require.NotNil(t, sess, "expected Codex session to remain")
	if assert.NotNil(t, sess.SessionName, "expected renamed session_name") {
		assert.Equal(t, "Renamed title", *sess.SessionName)
	}

	msgs = fetchMessages(t, env.db, "codex:"+uuid)
	require.Len(t, msgs, 2)
	assert.NotEmpty(t, msgs[1].TokenUsage)
	assert.Equal(t, 250, msgs[1].OutputTokens)
	assert.Equal(t, 100_000, msgs[1].ContextTokens)
}

// TestIncrementalSync_CodexIndexRenameBelowStoredMtimeRefreshesName covers a
// stale-title race introduced by folding the session_index.jsonl mtime into the
// incremental write's stored file_mtime. The initial parse stores a high
// effective mtime (the index mtime is newer than the transcript). A later index
// rename whose mtime is <= that stored value cannot be detected by a
// indexMtime > storedMtime gate, so the incremental path must compare the
// session name directly and fall back to a full parse, otherwise
// shouldSkipCodexFingerprint's storedMtime==effectiveMtime fast path would
// strand the stale title on every subsequent sync.
func TestIncrementalSync_CodexIndexRenameBelowStoredMtimeRefreshesName(t *testing.T) {
	root := t.TempDir()
	codexDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	env := setupTestEnv(t, WithCodexDirs([]string{codexDir}))

	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e1"
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/tmp/proj", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextJSON("gpt-5.5", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "first", tsEarlyS1),
	)
	path := env.writeCodexSession(
		t, filepath.Join("2024", "01", "01"),
		"rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
		initial,
	)

	indexPath := filepath.Join(root, "session_index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Original title","updated_at":"2024-01-01T10:00:00Z"}`+"\n",
		uuid,
	), 0o644))

	// The transcript is older than the index, so the initial parse stores the
	// index mtime as the effective file_mtime.
	transcriptTime := time.Now().Add(-2 * time.Hour)
	highIndexTime := time.Now().Add(-30 * time.Minute)
	require.NoError(t, os.Chtimes(path, transcriptTime, transcriptTime), "chtimes initial session")
	require.NoError(t, os.Chtimes(indexPath, highIndexTime, highIndexTime), "chtimes initial index")

	env.engine.SyncAll(context.Background(), nil)
	sess, err := env.db.GetSessionFull(context.Background(), "codex:"+uuid)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, sess, "expected Codex session to sync")
	require.NotNil(t, sess.SessionName, "expected session_name to be imported")
	require.Equal(t, "Original title", *sess.SessionName)
	require.NotNil(t, sess.FileMtime, "expected stored file_mtime")
	require.Equal(t, highIndexTime.UnixNano(), *sess.FileMtime,
		"expected stored file_mtime to fold in the higher index mtime")

	// Append to the transcript (an incremental update) and rename the index with
	// an mtime BELOW the stored file_mtime. effectiveMtime never exceeds the
	// stored value, so only a direct session-name comparison can catch the
	// rename.
	appended := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("assistant", "second", tsEarlyS5),
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	require.NoError(t, err, "append")
	require.NoError(t, f.Close())

	appendTime := time.Now().Add(-90 * time.Minute)
	lowIndexTime := time.Now().Add(-45 * time.Minute)
	require.NoError(t, os.Chtimes(path, appendTime, appendTime), "chtimes appended session")
	require.NoError(t, os.WriteFile(indexPath, fmt.Appendf(nil,
		`{"id":"%s","thread_name":"Renamed title","updated_at":"2024-01-01T10:01:00Z"}`+"\n",
		uuid,
	), 0o644))
	require.NoError(t, os.Chtimes(indexPath, lowIndexTime, lowIndexTime), "chtimes renamed index")

	env.engine.SyncPaths([]string{path})

	sess, err = env.db.GetSessionFull(context.Background(), "codex:"+uuid)
	require.NoError(t, err, "GetSessionFull after rename")
	require.NotNil(t, sess, "expected Codex session to remain")
	if assert.NotNil(t, sess.SessionName, "expected renamed session_name") {
		assert.Equal(t, "Renamed title", *sess.SessionName)
	}
	// The incremental append must still have landed.
	msgs := fetchMessages(t, env.db, "codex:"+uuid)
	require.Len(t, msgs, 2)
}

func TestIncrementalSync_CodexSubagentAppendFallsBackToFullParse(t *testing.T) {
	env := setupTestEnv(t)

	childID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"inc-cx-sub", "/tmp/proj",
			"codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "run child", tsEarlyS1),
		testjsonl.CodexFunctionCallWithCallIDJSON("spawn_agent", "call_spawn", map[string]any{
			"agent_type": "awaiter",
			"message":    "run it",
		}, tsEarlyS5),
		testjsonl.CodexFunctionCallOutputJSON("call_spawn", `{"agent_id":"`+childID+`","nickname":"Fennel"}`, "2024-01-01T10:01:00Z"),
	)
	path := env.writeCodexSession(
		t, filepath.Join("2024", "01", "01"),
		"rollout-20240101-inc-cx-sub.jsonl", initial,
	)
	env.engine.SyncAll(context.Background(), nil)

	assertSessionMessageCount(
		t, env.db, "codex:inc-cx-sub", 2,
	)

	appended := testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallWithCallIDJSON("wait", "call_wait", map[string]any{
			"ids": []string{childID},
		}, "2024-01-01T10:01:06Z"),
		testjsonl.CodexFunctionCallOutputJSON("call_wait",
			"{\"status\":{\""+childID+"\":{\"completed\":\"Finished successfully\"}}}",
			"2024-01-01T10:01:07Z",
		),
	)
	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	f.Close()
	require.NoError(t, err, "append")

	// SyncPaths hits the incremental Codex path first. The appended
	// wait call is an explicit full-parse fallback case and should
	// still produce the final parsed state successfully.
	env.engine.SyncPaths([]string{path})

	assertSessionMessageCount(
		t, env.db, "codex:inc-cx-sub", 3,
	)
	msgs := fetchMessages(t, env.db, "codex:inc-cx-sub")
	require.Equal(t, 3, len(msgs), "messages len = %d, want 3", len(msgs))
	require.Equal(t, 1, len(msgs[2].ToolCalls), "tool calls len = %d, want 1", len(msgs[2].ToolCalls))
	waitCall := msgs[2].ToolCalls[0]
	require.Equal(t, "wait", waitCall.ToolName, "tool name = %q, want %q", waitCall.ToolName, "wait")
	require.Equal(t, 1, len(waitCall.ResultEvents), "result events len = %d, want 1", len(waitCall.ResultEvents))
	require.Equal(t, childID, waitCall.ResultEvents[0].AgentID, "event agent_id = %q, want %q", waitCall.ResultEvents[0].AgentID, childID)
	require.Equal(t, "Finished successfully", waitCall.ResultEvents[0].Content, "event content = %q, want %q", waitCall.ResultEvents[0].Content, "Finished successfully")
	require.Equal(t, "Finished successfully", waitCall.ResultContent, "result_content = %q, want %q", waitCall.ResultContent, "Finished successfully")
}

func TestResyncAllCancelledPreservesOriginalDB(t *testing.T) {
	env := setupTestEnv(t)

	// Seed the DB with a session via Claude JSONL.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "hello").
		AddClaudeAssistant(tsEarlyS5, "world").
		String()
	env.writeClaudeSession(
		t, "cancel-project", "cancel-sess.jsonl", content,
	)
	env.engine.SyncAll(context.Background(), nil)

	// Verify session exists with messages.
	sess, err := env.db.GetSession(
		context.Background(), "cancel-sess",
	)
	require.NoError(t, err, "session not found")
	require.NotNil(t, sess, "session not found")
	origCount := sess.MessageCount
	require.NotZero(t, origCount, "expected messages after initial sync")

	// Cancel the context before starting ResyncAll so
	// collectAndBatch aborts immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stats := env.engine.ResyncAll(ctx, nil)

	require.True(t, stats.Aborted, "expected ResyncAll to report Aborted")

	// Original DB should be preserved — session still
	// has the original data.
	sess, err = env.db.GetSession(
		context.Background(), "cancel-sess",
	)
	require.NoError(t, err, "session lost after cancelled resync")
	require.NotNil(t, sess, "session lost after cancelled resync")
	assert.Equal(t, origCount, sess.MessageCount, "message count = %d, want %d", sess.MessageCount, origCount)
}

func TestSyncAllCancelledDoesNotUpdateLastSync(t *testing.T) {
	env := setupTestEnv(t)

	// Seed the DB with a session.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "hello").
		String()
	env.writeClaudeSession(
		t, "ls-project", "ls-sess.jsonl", content,
	)

	// Run a successful sync to set lastSync.
	env.engine.SyncAll(context.Background(), nil)
	lastSync := env.engine.LastSync()
	require.False(t, lastSync.IsZero(), "expected lastSync to be set")
	lastStats := env.engine.LastSyncStats()
	require.NotZero(t, lastStats.Synced, "expected synced > 0")

	// Run a cancelled sync.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats := env.engine.SyncAll(ctx, nil)
	require.True(t, stats.Aborted, "expected SyncAll to report Aborted")

	// lastSync and lastSyncStats should be unchanged.
	assert.Equal(t, lastSync, env.engine.LastSync(), "lastSync was updated by cancelled sync")
	assert.Equal(t, lastStats.Synced, env.engine.LastSyncStats().Synced, "lastSyncStats was updated by cancelled sync")
}

func TestSyncAllSince_FiltersByMtime(t *testing.T) {
	env := setupTestEnv(t)

	// Seed the DB with two sessions.
	oldContent := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "old session").
		String()
	oldPath := env.writeClaudeSession(
		t, "proj-old", "old-sess.jsonl", oldContent,
	)

	newContent := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "new session").
		String()
	newPath := env.writeClaudeSession(
		t, "proj-new", "new-sess.jsonl", newContent,
	)

	// Backdate the old file to simulate an unchanged prior
	// session; keep the new file at its natural mtime.
	longAgo := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(oldPath, longAgo, longAgo), "chtimes old")

	// SyncAllSince with a cutoff 1 hour ago should only
	// process the new file.
	cutoff := time.Now().Add(-1 * time.Hour)
	stats := env.engine.SyncAllSince(
		context.Background(), cutoff, nil,
	)
	assert.Equal(t, 1, stats.Synced, "synced = %d, want 1", stats.Synced)

	// Verify only the new session is in the DB.
	page, err := env.db.ListSessions(
		context.Background(), db.SessionFilter{Limit: 10},
	)
	require.NoError(t, err, "list sessions")
	require.Equal(t, 1, len(page.Sessions), "sessions = %d, want 1", len(page.Sessions))

	// Second call with zero cutoff syncs everything.
	stats = env.engine.SyncAllSince(
		context.Background(), time.Time{}, nil,
	)
	// The new file is already in the DB (skip cache);
	// the old file should now be synced too.
	assert.NotZero(t, stats.Synced, "expected second sync to pick up backdated file")

	page, err = env.db.ListSessions(
		context.Background(), db.SessionFilter{Limit: 10},
	)
	require.NoError(t, err, "list sessions")
	assert.Equal(t, 2, len(page.Sessions), "sessions = %d, want 2", len(page.Sessions))

	_ = newPath
}

func TestSyncRootsSinceScopesDiscoveredFiles(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	env := setupTestEnv(t, WithClaudeDirs([]string{rootA, rootB}))

	contentA := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "root a").
		String()
	pathA := env.writeSession(
		t, rootA, filepath.Join("proj-a", "root-a.jsonl"), contentA,
	)

	contentB := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "root b").
		String()
	pathB := env.writeSession(
		t, rootB, filepath.Join("proj-b", "root-b.jsonl"), contentB,
	)

	longAgo := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(pathA, longAgo, longAgo), "chtimes root a")
	require.NoError(t, os.Chtimes(pathB, longAgo, longAgo), "chtimes root b")

	cutoff := time.Now().Add(-1 * time.Hour)
	require.NoError(t, os.Chtimes(pathB, time.Now(), time.Now()), "touch root b")

	stats := env.engine.SyncRootsSince(
		context.Background(), []string{rootB}, cutoff, nil,
	)
	assert.Equal(t, 1, stats.TotalSessions, "total sessions")
	assert.Equal(t, 1, stats.Synced, "synced sessions")

	page, err := env.db.ListSessions(
		context.Background(), db.SessionFilter{Limit: 10},
	)
	require.NoError(t, err, "list sessions")
	require.Len(t, page.Sessions, 1, "sessions")
	require.NotNil(t, page.Sessions[0].FirstMessage, "first message")
	assert.Equal(t, "root b", *page.Sessions[0].FirstMessage)
}

func TestSyncRootsSinceScopesOpenCodeSQLiteRoots(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	env := setupTestEnv(t, WithOpenCodeDirs([]string{rootA, rootB}))

	ocA := createOpenCodeDB(t, rootA)
	ocA.addProject(t, "proj-a", "/home/user/code/root-a")
	ocA.addSession(t, "oc-root-a", "proj-a", 1704067200000, 1704067205000)
	ocA.addMessage(t, "msg-a-user", "oc-root-a", "user", 1704067200000)
	ocA.addTextPart(
		t, "part-a-user", "oc-root-a", "msg-a-user", "root a",
		1704067200000,
	)

	ocB := createOpenCodeDB(t, rootB)
	ocB.addProject(t, "proj-b", "/home/user/code/root-b")
	ocB.addSession(t, "oc-root-b", "proj-b", 1704067200000, 1704067205000)
	ocB.addMessage(t, "msg-b-user", "oc-root-b", "user", 1704067200000)
	ocB.addTextPart(
		t, "part-b-user", "oc-root-b", "msg-b-user", "root b",
		1704067200000,
	)

	stats := env.engine.SyncRootsSince(
		context.Background(), []string{rootB}, time.Time{}, nil,
	)
	assert.Equal(t, 1, stats.TotalSessions, "total sessions")
	assert.Equal(t, 1, stats.Synced, "synced sessions")

	page, err := env.db.ListSessions(
		context.Background(), db.SessionFilter{Limit: 10},
	)
	require.NoError(t, err, "list sessions")
	require.Len(t, page.Sessions, 1, "sessions")
	require.NotNil(t, page.Sessions[0].FirstMessage, "first message")
	assert.Equal(t, "root b", *page.Sessions[0].FirstMessage)
}

func TestSyncRootsSinceDoesNotAdvanceGlobalWatermark(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	env := setupTestEnv(t, WithClaudeDirs([]string{rootA, rootB}))

	watermark := time.Now().Add(-24 * time.Hour).UTC()
	require.NoError(t, env.db.SetSyncState(
		"last_sync_started_at",
		watermark.Format(time.RFC3339Nano),
	), "seed last_sync_started_at")

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "root a").
		String()
	pathA := env.writeSession(
		t, rootA, filepath.Join("proj-a", "root-a.jsonl"), content,
	)
	fileTime := watermark.Add(time.Hour)
	require.NoError(t, os.Chtimes(pathA, fileTime, fileTime), "chtimes root a")

	env.engine.SyncRootsSince(
		context.Background(), []string{rootB}, watermark, nil,
	)

	stats := env.engine.SyncAllSince(
		context.Background(), env.engine.LastSyncStartedAt(), nil,
	)
	assert.Equal(t, 1, stats.TotalSessions, "total sessions")
	assert.Equal(t, 1, stats.Synced, "synced sessions")

	page, err := env.db.ListSessions(
		context.Background(), db.SessionFilter{Limit: 10},
	)
	require.NoError(t, err, "list sessions")
	require.Len(t, page.Sessions, 1, "sessions")
	require.NotNil(t, page.Sessions[0].FirstMessage, "first message")
	assert.Equal(t, "root a", *page.Sessions[0].FirstMessage)
}

func TestSyncAll_PersistsStartedAndFinishedAt(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "hello").
		String()
	env.writeClaudeSession(
		t, "proj", "sess.jsonl", content,
	)

	before := time.Now().UTC().Add(-1 * time.Second)
	env.engine.SyncAll(context.Background(), nil)
	after := time.Now().UTC().Add(1 * time.Second)

	startedAt := env.engine.LastSyncStartedAt()
	require.False(t, startedAt.IsZero(), "LastSyncStartedAt is zero after sync")
	assert.False(t, startedAt.Before(before) || startedAt.After(after),
		"LastSyncStartedAt %v outside [%v, %v]", startedAt, before, after)

	finishedRaw, err := env.db.GetSyncState(
		"last_sync_finished_at",
	)
	require.NoError(t, err, "get finish state")
	require.NotEmpty(t, finishedRaw, "last_sync_finished_at not persisted")
}

func TestResyncAllPreservesPGPushMarkerID(t *testing.T) {
	env := setupTestEnv(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "hello").
		AddClaudeAssistant(tsZeroS5, "hi").
		String()
	env.writeClaudeSession(t, "proj", "sess.jsonl", content)
	env.engine.SyncAll(context.Background(), nil)

	require.NoError(t, env.db.SetSyncState("pg_push_marker_id", "marker-123"),
		"SetSyncState pg_push_marker_id")

	stats := env.engine.ResyncAll(context.Background(), nil)
	require.False(t, stats.Aborted, "ResyncAll aborted: %+v", stats)

	got, err := env.db.GetSyncState("pg_push_marker_id")
	require.NoError(t, err, "GetSyncState pg_push_marker_id after resync")
	assert.Equal(t, "marker-123", got)
}

func TestOpenCodeExcludedSessionsAreSkipped(t *testing.T) {

	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)

	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj1", "/tmp/proj1")
	oc.addSession(t, "oc-excl-sync", "proj1", 1000, 1000)
	oc.addMessage(t, "msg-sync", "oc-excl-sync", "user", 1000)
	oc.addTextPart(
		t, "part-sync", "oc-excl-sync", "msg-sync", "hi", 1000,
	)
	oc.addSession(t, "oc-excl-single", "proj1", 1000, 1000)
	oc.addMessage(
		t, "msg-single", "oc-excl-single", "user", 1000,
	)
	oc.addTextPart(
		t, "part-single", "oc-excl-single", "msg-single",
		"hello", 1000,
	)

	env.engine.SyncAll(context.Background(), nil)

	syncSessionID := "opencode:oc-excl-sync"
	singleSessionID := "opencode:oc-excl-single"
	assertSessionMessageCount(t, env.db, syncSessionID, 1)
	assertSessionMessageCount(t, env.db, singleSessionID, 1)

	// Permanently delete the session (marks it excluded).
	require.NoError(t, env.db.DeleteSession(syncSessionID), "delete sync session")
	require.NoError(t, env.db.DeleteSession(singleSessionID), "delete single session")

	// Bump time_updated so the parser would normally pick them up.
	oc.updateSessionTime(t, "oc-excl-sync", 2000)
	oc.updateSessionTime(t, "oc-excl-single", 2000)

	// Sync again: excluded sessions should not be counted as failures.
	stats := env.engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 0, stats.Failed,
		"excluded OpenCode session should not count as failure")

	require.NoError(t, env.engine.SyncSingleSession(singleSessionID),
		"SyncSingleSession on excluded OpenCode session returned error")
}

// TestSyncSingleSessionDeletedClaudeIsNoOp verifies that calling
// SyncSingleSession on permanently deleted or soft-deleted Claude sessions
// returns nil, not an error.
func TestSyncSingleSessionDeletedClaudeIsNoOp(t *testing.T) {

	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "hello").
		AddClaudeAssistant(tsZeroS5, "hi").
		String()

	env.writeClaudeSession(
		t, "test-proj", "excl-single.jsonl", content,
	)
	env.writeClaudeSession(
		t, "test-proj", "trashed-single.jsonl", content,
	)

	env.engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, env.db, "excl-single", 2)
	assertSessionMessageCount(t, env.db, "trashed-single", 2)

	// Permanently delete → marks it excluded.
	err := env.db.DeleteSession(
		"excl-single",
	)
	require.NoError(t, err, "DeleteSession")
	require.NoError(t, env.db.SoftDeleteSession("trashed-single"), "SoftDeleteSession")

	require.NoError(t, env.engine.SyncSingleSession("excl-single"),
		"SyncSingleSession on excluded session returned error")
	require.NoError(t, env.engine.SyncSingleSession("trashed-single"),
		"SyncSingleSession on trashed session returned error")
}

func TestSyncAllTrashedSessionIsSkippedAndCached(t *testing.T) {

	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "hello").
		AddClaudeAssistant(tsZeroS5, "hi").
		String()

	path := env.writeClaudeSession(
		t, "test-proj", "trashed-sync.jsonl", content,
	)
	env.engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, env.db, "trashed-sync", 2)

	require.NoError(t, env.db.SoftDeleteSession("trashed-sync"), "SoftDeleteSession")
	require.NoError(t, env.db.ResetAllMtimes(), "ResetAllMtimes")

	updated := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "hello again with a longer prompt").
		AddClaudeAssistant(tsZeroS5, "still here with a longer reply").
		String()
	require.NoError(t, os.Remove(path), "Remove")
	dbtest.WriteTestFile(t, path, []byte(updated))
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(path, future, future), "Chtimes")

	stats := env.engine.SyncAll(context.Background(), nil)
	require.Equal(t, 0, stats.Failed, "Failed = %d, want 0 for trashed session", stats.Failed)
	require.Equal(t, 0, stats.Synced, "Synced = %d, want 0 for trashed session", stats.Synced)
	require.NotZero(t, env.engine.SnapshotSkipCache()[path], "skip cache missing trashed session path %s", path)
}

func TestSyncAllTrashedSessionAppendUsesSkipPath(t *testing.T) {

	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "hello").
		AddClaudeAssistant(tsZeroS5, "hi").
		String()

	path := env.writeClaudeSession(
		t, "test-proj", "trashed-append.jsonl", content,
	)
	env.engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, env.db, "trashed-append", 2)

	require.NoError(t, env.db.SoftDeleteSession("trashed-append"), "SoftDeleteSession")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open append")
	_, err = f.WriteString(
		testjsonl.NewSessionBuilder().
			AddClaudeUser(tsEarly, "new prompt").
			AddClaudeAssistant(tsEarlyS5, "new reply").
			String(),
	)
	require.NoError(t, f.Close(), "close append")
	require.NoError(t, err, "append")

	stats := env.engine.SyncAll(context.Background(), nil)
	require.Equal(t, 0, stats.Failed, "Failed = %d, want 0 for trashed append", stats.Failed)
	require.Equal(t, 0, stats.Synced, "Synced = %d, want 0 for trashed append", stats.Synced)
	full, err := env.db.GetSessionFull(
		context.Background(), "trashed-append",
	)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, full, "MessageCount = nil, want preserved count 2")
	require.Equal(t, 2, full.MessageCount, "MessageCount = %v, want preserved count 2", full)
}

func TestIncrementalSync_ClaudeClearOnlyRepairedOnAppend(t *testing.T) {
	env := setupTestEnv(t)

	// Initial sync: session opens with only a /clear command
	// envelope. Under the new parser rule, first_message is
	// empty even though UserMsgCount is 1.
	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON(
			"<command-name>/clear</command-name>",
			tsZero,
		),
	)
	path := env.writeClaudeSession(
		t, "proj", "clear-only.jsonl", initial,
	)
	env.engine.SyncAll(context.Background(), nil)

	full, err := env.db.GetSessionFull(
		context.Background(), "clear-only",
	)
	require.NoError(t, err, "GetSessionFull after initial sync")
	if full.FirstMessage != nil {
		require.Equal(t, "", *full.FirstMessage, "initial FirstMessage = %q, want empty", *full.FirstMessage)
	}
	require.Equal(t, 1, full.UserMessageCount, "initial UserMessageCount = %d, want 1", full.UserMessageCount)

	// Append a real user message — incremental sync must now
	// fall back to a full parse so first_message gets populated.
	appended := testjsonl.ClaudeUserJSON(
		"Fix the login bug", tsZeroS1,
	) + "\n"
	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	f.Close()
	require.NoError(t, err, "append")

	env.engine.SyncPaths([]string{path})

	updated, err := env.db.GetSessionFull(
		context.Background(), "clear-only",
	)
	require.NoError(t, err, "GetSessionFull after append")
	require.NotNil(t, updated.FirstMessage,
		"FirstMessage after append = nil, want %q", "Fix the login bug")
	assert.Equal(t, "Fix the login bug", *updated.FirstMessage,
		"FirstMessage after append = %q, want %q", *updated.FirstMessage, "Fix the login bug")
	assert.Equal(t, 2, updated.UserMessageCount, "UserMessageCount after append = %d, want 2", updated.UserMessageCount)
}

// TestIncrementalSync_CodexReemittedPromptDedupedOnAppend covers
// the case where Codex re-emits the initial prompt verbatim on a
// continued turn. The duplicate is appended after the first sync,
// so the incremental parser starts past the original prompt and
// only sees the re-emitted copy. It must still recognise it as a
// duplicate via the stored first_message rather than counting it
// as a second user turn — otherwise an automated session (here a
// roborev code review) flips to UserMessageCount 2 and loses its
// is_automated classification, resurfacing in the viewer.
func TestIncrementalSync_CodexReemittedPromptDedupedOnAppend(t *testing.T) {
	env := setupTestEnv(t)

	uuid := "c3d4e5f6-3456-789a-bcde-f01234567890"
	prompt := "You are a code reviewer. Review the code changes shown below."

	initial := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", prompt).
		AddCodexMessage(tsEarlyS5, "assistant", "Looks good.").
		String()
	path := env.writeCodexSession(
		t, filepath.Join("2024", "01", "15"),
		"rollout-20240115-"+uuid+".jsonl", initial,
	)
	env.engine.SyncAll(context.Background(), nil)

	sessionID := "codex:" + uuid
	full, err := env.db.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err, "GetSessionFull after initial sync")
	require.Equal(t, 1, full.UserMessageCount,
		"initial UserMessageCount = %d, want 1", full.UserMessageCount)
	require.True(t, full.IsAutomated,
		"initial IsAutomated = false, want true for reviewer prompt")

	// Codex continues after an interrupted turn, re-emits the
	// same prompt verbatim, then adds a fresh assistant reply.
	appended := testjsonl.CodexMsgJSON(
		"user", "<turn_aborted>\ninterrupted", "2024-01-01T10:00:59Z",
	) + "\n" + testjsonl.CodexMsgJSON(
		"user", prompt, "2024-01-01T10:01:00Z",
	) + "\n" + testjsonl.CodexMsgJSON(
		"assistant", "Still good.", "2024-01-01T10:01:05Z",
	) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	f.Close()
	require.NoError(t, err, "append")

	env.engine.SyncPaths([]string{path})

	updated, err := env.db.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err, "GetSessionFull after append")
	assert.Equal(t, 1, updated.UserMessageCount,
		"UserMessageCount after re-emitted prompt = %d, want 1 (duplicate must be dropped)",
		updated.UserMessageCount)
	assert.Equal(t, 3, updated.MessageCount,
		"MessageCount after append = %d, want 3 (only the assistant reply is new)",
		updated.MessageCount)
	assert.True(t, updated.IsAutomated,
		"IsAutomated after append = false, want true (dedup must preserve classification)")
}

func testStringPtrValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
