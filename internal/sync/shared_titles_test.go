package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

// sharedTitleScopeRecorder records the refresh scopes one engine emits so a
// test can tell a title-only pass that notified clients from one that wrote
// nothing at all.
type sharedTitleScopeRecorder struct {
	mu     sync.Mutex
	scopes []string
}

func (r *sharedTitleScopeRecorder) Emit(scope string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scopes = append(r.scopes, scope)
}

func (r *sharedTitleScopeRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.scopes...)
	r.scopes = nil
	return out
}

func sharedTitleTestEngine(
	t *testing.T, emitter Emitter,
) (*Engine, *db.DB) {
	t.Helper()
	return sharedTitleTestEngineForDirs(
		t, emitter, map[parser.AgentType][]string{})
}

func sharedTitleTestEngineForDirs(
	t *testing.T, emitter Emitter, dirs map[parser.AgentType][]string,
) (*Engine, *db.DB) {
	t.Helper()
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: dirs,
		Machine:   "devbox",
		Emitter:   emitter,
	})
	t.Cleanup(engine.Close)
	return engine, database
}

// seedSharedTitleSession writes one stored session row directly, so the title
// refresh has an existing name and a source path to check ownership against.
// The source path is placed under root, which for these fixtures is also the
// directory holding the shared title database, so ownership resolves to that
// database. A test that needs the two to disagree uses
// seedSharedTitleSessionAt or a different root.
func seedSharedTitleSession(
	t *testing.T, database *db.DB, id string, agent parser.AgentType,
	name, root string,
) {
	t.Helper()
	bareID := strings.TrimPrefix(id, string(agent)+":")
	source := filepath.Join(root, "conversations", bareID+".db")
	if agent == parser.AgentQoder {
		source = filepath.Join(root, "projects", bareID+".jsonl")
	}
	seedSharedTitleSessionAt(t, database, id, agent, name, source)
}

// seedSharedTitleSessionAt writes a session row with an explicit source path.
func seedSharedTitleSessionAt(
	t *testing.T, database *db.DB, id string, agent parser.AgentType,
	name, sourcePath string,
) {
	t.Helper()
	var stored *string
	if name != "" {
		clean := name
		stored = &clean
	}
	var filePath *string
	if sourcePath != "" {
		clean := sourcePath
		filePath = &clean
	}
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: id, Agent: string(agent), SessionName: stored, FilePath: filePath,
	}))
}

func requireStoredTitle(
	t *testing.T, database *db.DB, id, want string,
) {
	t.Helper()
	name, found, err := database.GetSessionName(t.Context(), id)
	require.NoError(t, err)
	require.True(t, found, "session %s should exist", id)
	assert.Equal(t, want, name)
}

// writeSharedTitleDB creates a shared title database fixture. table picks the
// provider's schema so the same helper serves Antigravity and Qoder.
func writeSharedTitleDB(
	t *testing.T, path, table, idColumn string, titles map[string]*string,
) {
	t.Helper()
	dbtest.WriteTestFile(t, path, nil)
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(),
		"CREATE TABLE "+table+" ("+idColumn+" text PRIMARY KEY, title text)")
	require.NoError(t, err)
	for id, title := range titles {
		_, err := conn.ExecContext(t.Context(),
			"INSERT INTO "+table+" ("+idColumn+", title) VALUES (?, ?)",
			id, title)
		require.NoError(t, err)
	}
}

// writeConversationSummariesDB is writeSharedTitleDB for the Antigravity store.
func writeConversationSummariesDB(
	t *testing.T, root string, titles map[string]*string,
) string {
	t.Helper()
	path := parser.AntigravityTitleDatabasePath(root)
	writeSharedTitleDB(t, path, "conversation_summaries", "conversation_id", titles)
	return path
}

// writeQoderSessionsDB is writeSharedTitleDB for a Qoder application database.
// app is the application directory name under the fixture base.
func writeQoderSessionsDB(
	t *testing.T, base, app string, titles map[string]*string,
) string {
	t.Helper()
	t.Setenv("HOME", base)
	t.Setenv("USERPROFILE", base)
	path := filepath.Join(base, "Library", "Application Support", app, "main.sqlite")
	writeSharedTitleDB(t, path, "chat_sessions", "session_id", titles)
	return path
}

// TestPlanClassifiesSharedTitleDatabaseAsTitleTaskOnly pins the separation the
// whole design rests on: a shared title database event is claimed as a refresh
// task and contributes no DiscoveredFile, so a rename can never turn into a
// transcript reparse that rewrites messages and token rows.
func TestPlanClassifiesSharedTitleDatabaseAsTitleTaskOnly(t *testing.T) {
	engine, _ := sharedTitleTestEngine(t, nil)
	titlePath := writeConversationSummariesDB(t, t.TempDir(), nil)

	files, titles, err := engine.classifyChangedPaths(t.Context(), []string{titlePath})
	require.NoError(t, err)
	assert.Empty(t, files, "a title write must not produce an import file")
	require.Len(t, titles, 1)
	assert.Equal(t, parser.AgentAntigravity, titles[0].Agent)
	assert.Equal(t, titlePath, titles[0].DBPath)
	assert.Equal(t, "antigravity:", titles[0].IDPrefix())

	// The -wal sibling is the same database, and the -shm index is never one:
	// every read-only open rewrites -shm, so honoring it would re-trigger the
	// refresh on each pass.
	files, titles, err = engine.classifyChangedPaths(
		t.Context(), []string{titlePath + "-wal", titlePath + "-shm"})
	require.NoError(t, err)
	assert.Empty(t, files)
	require.Len(t, titles, 1, "only the -wal event resolves; -shm is dropped")
	assert.Equal(t, titlePath, titles[0].DBPath)
}

// TestSharedTitleRefreshRewritesOnlyChangedNames covers the write side: only a
// row whose title differs from the stored value is written, a SQL NULL and an
// unimported id are no signal, and a title-only pass still notifies clients.
func TestSharedTitleRefreshRewritesOnlyChangedNames(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	engine, database := sharedTitleTestEngine(t, emitter)
	root := t.TempDir()

	const (
		changed    = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		unchanged  = "11111111-2222-4333-8444-555555555555"
		nullTitle  = "99999999-8888-4777-8666-555555555555"
		unimported = "12121212-3434-4565-8787-909090909090"
	)
	seedSharedTitleSession(t, database, "antigravity:"+changed,
		parser.AgentAntigravity, "old name", root)
	seedSharedTitleSession(t, database, "antigravity:"+unchanged,
		parser.AgentAntigravity, "same name", root)
	seedSharedTitleSession(t, database, "antigravity:"+nullTitle,
		parser.AgentAntigravity, "keep on null", root)

	renamed := "会话标题样例"
	same := "same name"
	titlePath := writeConversationSummariesDB(t, root, map[string]*string{
		changed:    &renamed,
		unchanged:  &same,
		nullTitle:  nil,
		unimported: &renamed,
	})

	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))

	requireStoredTitle(t, database, "antigravity:"+changed, renamed)
	requireStoredTitle(t, database, "antigravity:"+unchanged, same)
	requireStoredTitle(t, database, "antigravity:"+nullTitle, "keep on null")

	_, found, err := database.GetSessionName(t.Context(), "antigravity:"+unimported)
	require.NoError(t, err)
	assert.False(t, found,
		"a title refresh must never create a row for an unimported session")

	assert.Equal(t, []string{"sessions"}, emitter.take(),
		"a pass that changed a title must notify clients")

	// A second pass over the steady state writes nothing and stays silent.
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))
	assert.Empty(t, emitter.take(),
		"an unchanged title must not notify clients again")
}

// TestSharedTitleRefreshAppliesProviderIDPrefix pins the id translation: the
// shared database stores a bare id, but sessions.id is prefixed, so a bare id
// must never match a stored row.
func TestSharedTitleRefreshAppliesProviderIDPrefix(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	engine, database := sharedTitleTestEngine(t, emitter)
	base := t.TempDir()
	// The client directory is an ancestor of the session root, matching the
	// roots AgentsView actually configures (<client>/projects).
	root := filepath.Join(base, ".qoder")
	const bare = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "qoder:"+bare, parser.AgentQoder, "old", root)
	// A same-file fork is refreshed with the owner. This row only shares the
	// bare id, so it keeps a different source path and must stay untouched.
	seedSharedTitleSessionAt(t, database, bare, parser.AgentQoder,
		"unprefixed row", filepath.Join(root, "projects", "other.jsonl"))

	title := "Qoder 标题"
	titlePath := writeQoderSessionsDB(t, base, "com.qoder.app.stable",
		map[string]*string{bare: &title})

	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))

	requireStoredTitle(t, database, "qoder:"+bare, title)
	requireStoredTitle(t, database, bare, "unprefixed row")
	assert.Equal(t, []string{"sessions"}, emitter.take())
}

// TestSharedTitleRefreshBlankClearsAndMissingSignalPreserves covers the two
// ends of the four-state contract at the write site: an explicitly blank title
// clears the stored name, while a SQL NULL or a database with no row for the
// session leaves it untouched.
func TestSharedTitleRefreshBlankClearsAndMissingSignalPreserves(t *testing.T) {
	t.Run("explicit blank clears", func(t *testing.T) {
		emitter := &sharedTitleScopeRecorder{}
		engine, database := sharedTitleTestEngine(t, emitter)
		root := t.TempDir()
		const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		seedSharedTitleSession(t, database, "antigravity:"+id,
			parser.AgentAntigravity, "will be cleared", root)

		blank := ""
		titlePath := writeConversationSummariesDB(t, root,
			map[string]*string{id: &blank})

		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))
		requireStoredTitle(t, database, "antigravity:"+id, "")
		assert.Equal(t, []string{"sessions"}, emitter.take())
	})

	t.Run("no row for the session is no signal", func(t *testing.T) {
		emitter := &sharedTitleScopeRecorder{}
		engine, database := sharedTitleTestEngine(t, emitter)
		root := t.TempDir()
		const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		seedSharedTitleSession(t, database, "antigravity:"+id,
			parser.AgentAntigravity, "keep me", root)

		other := "someone else"
		titlePath := writeConversationSummariesDB(t, root,
			map[string]*string{
				"12121212-3434-4565-8787-909090909090": &other,
			})

		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))
		requireStoredTitle(t, database, "antigravity:"+id, "keep me")
		assert.Empty(t, emitter.take(), "no signal must not notify clients")
	})
}

// TestSharedTitleRefreshReadFailureKeepsNamesAndRetries pins the failure
// contract: an unreadable database is not a parse failure, every stored name
// survives, and nothing is written that a later pass would have to undo.
func TestSharedTitleRefreshReadFailureKeepsNamesAndRetries(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	engine, database := sharedTitleTestEngine(t, emitter)
	root := t.TempDir()
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "antigravity:"+id,
		parser.AgentAntigravity, "survives a broken store", root)

	titlePath := parser.AntigravityTitleDatabasePath(root)
	dbtest.WriteTestFile(t, titlePath, []byte("not a database"))

	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))
	requireStoredTitle(t, database, "antigravity:"+id, "survives a broken store")
	assert.Empty(t, emitter.take())
}

// TestSharedTitleRefreshAndDeletionInOneBatchBothApply guards the boundary the
// title task introduced: one batch can carry a shared title database event and
// a deleted session path together. A title refresh produces no import file, so
// an early exit keyed on "no files" would silently drop the deletion and leave
// the vanished session looking present.
func TestSharedTitleRefreshAndDeletionInOneBatchBothApply(t *testing.T) {
	const agent parser.AgentType = "changed-path-title-and-delete"
	emitter := &sharedTitleScopeRecorder{}
	database, engine, provider, _, deletedPath := newChangedPathOutcomeEngine(
		t, agent, func(string) parser.ParseOutcome { return parser.ParseOutcome{} },
	)
	engine.emitter = emitter

	seedActiveBaselineSource(t, database, agent, "titled-and-deleted", deletedPath)
	require.NoError(t, database.BaselineActiveSessionSourcePaths(
		t.Context(), "local",
		[]db.SessionSourcePath{{Agent: string(agent), FilePath: deletedPath}},
	))
	titleRoot := t.TempDir()
	const titleID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "antigravity:"+titleID,
		parser.AgentAntigravity, "before rename", titleRoot)

	renamed := "重命名后的标题"
	titlePath := writeConversationSummariesDB(t, titleRoot,
		map[string]*string{titleID: &renamed})

	require.NoError(t, os.Remove(deletedPath))
	provider.source = nil
	require.NoError(t, engine.SyncPathsContext(
		t.Context(), []string{deletedPath, titlePath}))

	requireStoredTitle(t, database, "antigravity:"+titleID, renamed)
	gone, err := database.GetSessionFull(t.Context(), "titled-and-deleted")
	require.NoError(t, err)
	require.NotNil(t, gone)
	assert.NotNil(t, gone.SourceMissingAt,
		"the deletion in the same batch must still be tombstoned")
	assert.Equal(t, []string{"sessions"}, emitter.take())
}

// TestSharedTitleRefreshRespectsSourceOwnership pins the ownership gate. A
// shared database is authoritative only for sessions that belong to the client
// owning it and whose source actually lives where that database's client keeps
// its transcripts. A row for a session stored elsewhere must not rewrite that
// session's title.
func TestSharedTitleRefreshRespectsSourceOwnership(t *testing.T) {
	t.Run("antigravity row from another root is ignored", func(t *testing.T) {
		emitter := &sharedTitleScopeRecorder{}
		engine, database := sharedTitleTestEngine(t, emitter)
		titleRoot := t.TempDir()
		const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		seedSharedTitleSession(t, database, "antigravity:"+id,
			parser.AgentAntigravity, "keep me", t.TempDir())

		renamed := "must not be applied"
		titlePath := writeConversationSummariesDB(t, titleRoot,
			map[string]*string{id: &renamed})

		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))
		requireStoredTitle(t, database, "antigravity:"+id, "keep me")
		assert.Empty(t, emitter.take())
	})

	t.Run("qoder row for the other client's session is ignored", func(t *testing.T) {
		emitter := &sharedTitleScopeRecorder{}
		engine, database := sharedTitleTestEngine(t, emitter)
		base := t.TempDir()
		const bare = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		// The session is a CN session, but the database being refreshed
		// belongs to the international build.
		seedSharedTitleSession(t, database, "qoder:"+bare, parser.AgentQoder,
			"CN 会话原标题", filepath.Join(base, ".qoder-cn"))

		title := "国际库标题"
		titlePath := writeQoderSessionsDB(t, base, "com.qoder.app.stable",
			map[string]*string{bare: &title})

		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))
		requireStoredTitle(t, database, "qoder:"+bare, "CN 会话原标题")
		assert.Empty(t, emitter.take())
	})

	t.Run("session without a stored source is ignored", func(t *testing.T) {
		emitter := &sharedTitleScopeRecorder{}
		engine, database := sharedTitleTestEngine(t, emitter)
		root := t.TempDir()
		const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		seedSharedTitleSessionAt(t, database, "antigravity:"+id,
			parser.AgentAntigravity, "keep me", "")

		renamed := "must not be applied"
		titlePath := writeConversationSummariesDB(t, root,
			map[string]*string{id: &renamed})

		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))
		requireStoredTitle(t, database, "antigravity:"+id, "keep me")
		assert.Empty(t, emitter.take())
	})
}

// TestSharedTitleRefreshDefersToSiblingJSON pins the priority rule that keeps
// the parse path and this refresh from fighting. A non-empty sibling
// "<uuid>-session.json" outranks the Qoder application database, so a refresh
// must leave the row alone; otherwise the two would trade the title back and
// forth on every pass.
func TestSharedTitleRefreshDefersToSiblingJSON(t *testing.T) {
	const bare = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

	t.Run("JSON title wins and stays", func(t *testing.T) {
		emitter := &sharedTitleScopeRecorder{}
		engine, database := sharedTitleTestEngine(t, emitter)
		base := t.TempDir()
		root := filepath.Join(base, ".qoder")
		jsonTitle := "JSON 标题优先"
		seedSharedTitleSession(t, database, "qoder:"+bare,
			parser.AgentQoder, jsonTitle, root)
		dbtest.WriteTestFile(t,
			filepath.Join(root, "projects", bare+"-session.json"),
			[]byte(`{"title":"JSON 标题优先"}`))

		dbTitle := "SQLite 标题"
		titlePath := writeQoderSessionsDB(t, base, "com.qoder.app.stable",
			map[string]*string{bare: &dbTitle})

		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))
		requireStoredTitle(t, database, "qoder:"+bare, jsonTitle)
		assert.Empty(t, emitter.take(),
			"a JSON-titled session must not be rewritten from the database")

		// Repeating the pass must not drift the title either.
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))
		requireStoredTitle(t, database, "qoder:"+bare, jsonTitle)
	})

	t.Run("unreadable sibling JSON blocks the write", func(t *testing.T) {
		emitter := &sharedTitleScopeRecorder{}
		engine, database := sharedTitleTestEngine(t, emitter)
		base := t.TempDir()
		root := filepath.Join(base, ".qoder")
		seedSharedTitleSession(t, database, "qoder:"+bare,
			parser.AgentQoder, "keep me", root)
		dbtest.WriteTestFile(t,
			filepath.Join(root, "projects", bare+"-session.json"),
			[]byte("{not json"))

		dbTitle := "must not overwrite an unreadable JSON"
		titlePath := writeQoderSessionsDB(t, base, "com.qoder.app.stable",
			map[string]*string{bare: &dbTitle})

		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))
		requireStoredTitle(t, database, "qoder:"+bare, "keep me")
		assert.Empty(t, emitter.take(),
			"a JSON that cannot be read is not permission to overwrite it")
	})
}

// TestSyncSharedTitlesCoversRenamesMissedWhileStopped covers the catch-up
// contract: the watcher only sees a rename that happens while AgentsView is
// running, so a sweep must reconcile from the stored rows with no filesystem
// event at all -- and must stay silent on the second pass.
func TestSyncSharedTitlesCoversRenamesMissedWhileStopped(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	root := t.TempDir()
	engine, database := sharedTitleTestEngineForDirs(t, emitter,
		map[parser.AgentType][]string{parser.AgentAntigravity: {root}})
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "antigravity:"+id,
		parser.AgentAntigravity, "stale name", root)

	renamed := "renamed while the app was closed"
	writeConversationSummariesDB(t, root, map[string]*string{id: &renamed})

	require.NoError(t, engine.SyncSharedTitlesContext(t.Context()))
	requireStoredTitle(t, database, "antigravity:"+id, renamed)
	assert.Equal(t, []string{"sessions"}, emitter.take())

	require.NoError(t, engine.SyncSharedTitlesContext(t.Context()))
	requireStoredTitle(t, database, "antigravity:"+id, renamed)
	assert.Empty(t, emitter.take(), "a steady state must not notify again")
}

// TestSyncSharedTitlesPicksUpDatabaseCreatedLater covers the compensation for
// a store that did not exist when the sweep started: the database list is
// recomputed every cycle, so no event and no restart is required.
func TestSyncSharedTitlesPicksUpDatabaseCreatedLater(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	root := t.TempDir()
	engine, database := sharedTitleTestEngineForDirs(t, emitter,
		map[parser.AgentType][]string{parser.AgentAntigravity: {root}})
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "antigravity:"+id,
		parser.AgentAntigravity, "stale name", root)

	// Nothing to read yet: this must be a silent no-op, not an error.
	require.NoError(t, engine.SyncSharedTitlesContext(t.Context()))
	requireStoredTitle(t, database, "antigravity:"+id, "stale name")
	assert.Empty(t, emitter.take())

	renamed := "store appeared later"
	writeConversationSummariesDB(t, root, map[string]*string{id: &renamed})

	require.NoError(t, engine.SyncSharedTitlesContext(t.Context()))
	requireStoredTitle(t, database, "antigravity:"+id, renamed)
	assert.Equal(t, []string{"sessions"}, emitter.take())
}

// TestSyncSharedTitlesKeepsNamesOnReadFailure pins that the sweep reports the
// failure to its caller while leaving every stored name intact for the next
// cycle, instead of failing or clearing anything.
func TestSyncSharedTitlesKeepsNamesOnReadFailure(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	root := t.TempDir()
	engine, database := sharedTitleTestEngineForDirs(t, emitter,
		map[parser.AgentType][]string{parser.AgentAntigravity: {root}})
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "antigravity:"+id,
		parser.AgentAntigravity, "keep me", root)
	dbtest.WriteTestFile(t, parser.AntigravityTitleDatabasePath(root),
		[]byte("not a database"))

	require.Error(t, engine.SyncSharedTitlesContext(t.Context()))
	requireStoredTitle(t, database, "antigravity:"+id, "keep me")
	assert.Empty(t, emitter.take())
}

// TestSyncSharedTitlesIsRefusedOnReportOnlyEngine keeps the write-refusal
// contract: a parse-diff engine never writes, and a title refresh is a write.
func TestSyncSharedTitlesIsRefusedOnReportOnlyEngine(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	root := t.TempDir()
	engine, database := sharedTitleTestEngineForDirs(t, emitter,
		map[parser.AgentType][]string{parser.AgentAntigravity: {root}})
	engine.forceParse = true
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "antigravity:"+id,
		parser.AgentAntigravity, "untouched", root)

	renamed := "must not be written"
	writeConversationSummariesDB(t, root, map[string]*string{id: &renamed})

	require.NoError(t, engine.SyncSharedTitlesContext(t.Context()))
	requireStoredTitle(t, database, "antigravity:"+id, "untouched")
	assert.Empty(t, emitter.take())
}

// TestSyncSharedTitlesWithoutConfiguredRootsIsANoOp keeps the sweep cheap on a
// machine with neither client: nothing to read, nothing written, no notify.
func TestSyncSharedTitlesWithoutConfiguredRootsIsANoOp(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	engine, _ := sharedTitleTestEngine(t, emitter)
	assert.Empty(t, engine.sharedTitleDatabases())
	require.NoError(t, engine.SyncSharedTitlesContext(t.Context()))
	assert.Empty(t, emitter.take())
}

// TestSharedTitleDatabasesForRootsExistence pins the enumeration contract:
// only databases that exist are returned, duplicates collapse, and a root that
// belongs to neither client contributes nothing.
func TestSharedTitleDatabasesForRootsExistence(t *testing.T) {
	root := t.TempDir()
	assert.Empty(t, parser.SharedTitleDatabasesForRoots(
		parser.AgentAntigravity, []string{root}),
		"an absent summaries database must not be enumerated")

	path := writeConversationSummariesDB(t, root, nil)
	got := parser.SharedTitleDatabasesForRoots(
		parser.AgentAntigravity, []string{root, root})
	require.Len(t, got, 1)
	assert.Equal(t, path, got[0].DBPath)
	assert.Equal(t, parser.AgentAntigravity, got[0].Agent)

	assert.Empty(t, parser.SharedTitleDatabasesForRoots(
		parser.AgentClaude, []string{root}))
	assert.Empty(t, parser.SharedTitleDatabasesForRoots(
		parser.AgentQoder, []string{root}),
		"a root that is not a recognized Qoder client directory binds nothing")

	// .qoderwork is a separate legacy export directory, so it must not be read
	// as .qoder and bind this machine's application database. (The positive
	// default-root case is pinned in the parser package, where no real
	// application database needs to exist.)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Empty(t, parser.SharedTitleDatabasesForRoots(parser.AgentQoder,
		[]string{filepath.Join(home, ".qoderwork", "projects")}))
}

// TestPlanSharedTitleTaskReachesExecution covers the two-phase contract end to
// end: planning claims the database and records a refresh task without writing
// anything, and the execution phase performs the write under the sync lock.
// This is the shape the desktop changed-path batch uses.
func TestPlanSharedTitleTaskReachesExecution(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	engine, database := sharedTitleTestEngine(t, emitter)
	root := t.TempDir()
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "antigravity:"+id,
		parser.AgentAntigravity, "before rename", root)

	renamed := "after rename"
	titlePath := writeConversationSummariesDB(t, root,
		map[string]*string{id: &renamed})

	plan, err := engine.PlanChangedPathsContext(t.Context(), []string{titlePath})
	require.NoError(t, err)
	assert.Empty(t, plan.Files, "planning must not project an import file")
	assert.Empty(t, plan.FallbackProviders)
	require.Len(t, plan.SharedTitleTasks, 1)
	requireStoredTitle(t, database, "antigravity:"+id, "before rename")
	assert.Empty(t, emitter.take(), "planning must never write or notify")

	result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
	require.NoError(t, err)
	requireStoredTitle(t, database, "antigravity:"+id, renamed)
	assert.Equal(t, 1, result.Stats.TitlesUpdated)
	assert.Equal(t, 0, result.FilesDiscovered)
	assert.Equal(t, 0, result.FilesProcessed)
	assert.Equal(t, []string{"sessions"}, emitter.take())
}

// TestSharedTitleDatabasesDedupeOnPlan keeps one refresh per database even
// when the event batch reports the same store several times, so a client that
// writes the WAL in bursts cannot fan a rename into repeated full scans.
func TestSharedTitleDatabasesDedupeOnPlan(t *testing.T) {
	engine, _ := sharedTitleTestEngine(t, nil)
	path := writeConversationSummariesDB(t, t.TempDir(), nil)

	_, titles, err := engine.classifyChangedPaths(
		t.Context(), []string{path, path, path + "-wal"})
	require.NoError(t, err)
	require.Len(t, titles, 1)
	assert.Equal(t, path, titles[0].DBPath)
	assert.False(t, strings.Contains(titles[0].DBPath, "-wal"),
		"a -wal event must resolve to the main database file")
}

func TestSharedTitleWALEventIsOneTitleTask(t *testing.T) {
	engine, _ := sharedTitleTestEngine(t, nil)
	titlePath := writeConversationSummariesDB(t, t.TempDir(), nil)
	wal := titlePath + "-wal"

	files, titles, err := engine.classifyChangedPaths(t.Context(), []string{wal})
	require.NoError(t, err)
	assert.Empty(t, files)
	require.Len(t, titles, 1)
	assert.Equal(t, titlePath, titles[0].DBPath)

	plan, err := engine.PlanChangedPathsContext(t.Context(), []string{wal})
	require.NoError(t, err)
	assert.Empty(t, plan.Files)
	assert.Empty(t, plan.FallbackProviders)
	require.Len(t, plan.SharedTitleTasks, 1)
	assert.Equal(t, titlePath, plan.SharedTitleTasks[0].DBPath)
}

func TestSharedTitleSHMEventIsNotATitleTask(t *testing.T) {
	for _, tc := range []struct {
		name      string
		agent     parser.AgentType
		clientDir string
		app       string
	}{
		{name: "antigravity", agent: parser.AgentAntigravity},
		{name: "qoder international", agent: parser.AgentQoder, clientDir: ".qoder", app: "com.qoder.app.stable"},
		{name: "qoder domestic", agent: parser.AgentQoder, clientDir: ".qoder-cn", app: "com.qodercn.app.stable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			root := home
			var titlePath string
			if tc.agent == parser.AgentQoder {
				root = filepath.Join(home, tc.clientDir, "projects")
				titlePath = writeQoderSessionsDB(t, home, tc.app, nil)
			} else {
				titlePath = writeConversationSummariesDB(t, root, nil)
			}
			require.NoError(t, os.MkdirAll(root, 0o755))
			engine, _ := sharedTitleTestEngineForDirs(t, nil,
				map[parser.AgentType][]string{tc.agent: {root}})
			shm := titlePath + "-shm"

			files, titles, err := engine.classifyChangedPaths(t.Context(), []string{shm})
			require.NoError(t, err)
			assert.Empty(t, files)
			assert.Empty(t, titles)
			files, err = engine.classifyProviderChangedPath(t.Context(), shm)
			require.NoError(t, err)
			assert.Empty(t, files)

			plan, err := engine.PlanChangedPathsContext(t.Context(), []string{shm})
			require.NoError(t, err)
			assert.Empty(t, plan.Files)
			assert.Empty(t, plan.FallbackProviders)
			assert.Empty(t, plan.SharedTitleTasks)

			// Other SQLite sidecars inside a configured root must still reach the
			// provider; ignoring every -shm path would hide session source changes.
			unrelated := filepath.Join(root, "other.sqlite-shm")
			plan, err = engine.PlanChangedPathsContext(t.Context(), []string{unrelated})
			require.NoError(t, err)
			assert.Equal(t, []parser.AgentType{tc.agent}, plan.FallbackProviders)
		})
	}
}
