package sync_test

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type omnigentParseCountingFactory struct {
	delegate parser.ProviderFactory
	count    *atomic.Int64
	results  *atomic.Int64
	failPath string
	failOnce *atomic.Bool
}

func (f omnigentParseCountingFactory) Definition() parser.AgentDef {
	return f.delegate.Definition()
}

func (f omnigentParseCountingFactory) Capabilities() parser.Capabilities {
	return f.delegate.Capabilities()
}

func (f omnigentParseCountingFactory) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	return &omnigentParseCountingProvider{
		Provider: f.delegate.NewProvider(cfg),
		count:    f.count,
		results:  f.results,
		failPath: f.failPath,
		failOnce: f.failOnce,
	}
}

type omnigentParseCountingProvider struct {
	parser.Provider
	count    *atomic.Int64
	results  *atomic.Int64
	failPath string
	failOnce *atomic.Bool
}

func (p *omnigentParseCountingProvider) StoredSourceHintScopes(
	req parser.ChangedPathRequest,
) []parser.StoredSourceHintScope {
	resolver, ok := p.Provider.(parser.StoredSourceHintScopeProvider)
	if !ok {
		return nil
	}
	return resolver.StoredSourceHintScopes(req)
}

func (p *omnigentParseCountingProvider) WatchRoots(
	ctx context.Context,
) ([]parser.WatchRoot, error) {
	planner, ok := p.Provider.(parser.WatchRootPlanner)
	if !ok {
		return nil, parser.UnsupportedProviderFeatureError{
			Provider: p.Definition().Type,
			Feature:  parser.ProviderFeatureWatchRoots,
		}
	}
	return planner.WatchRoots(ctx)
}

func (p *omnigentParseCountingProvider) RestoreCachedSourceState(
	ctx context.Context, source parser.SourceRef,
) (bool, error) {
	return parser.RestoreOmnigentCachedSourceState(ctx, p.Provider, source)
}

func (p *omnigentParseCountingProvider) PersistentArchiveSource(
	path string, fullSessionID string,
) (string, bool) {
	resolver, ok := p.Provider.(parser.PersistentArchiveSourceResolver)
	if !ok {
		return "", false
	}
	return resolver.PersistentArchiveSource(path, fullSessionID)
}

func (p *omnigentParseCountingProvider) DiscoverEach(
	ctx context.Context, yield func(parser.SourceRef) error,
) error {
	discoverer, ok := p.Provider.(parser.StreamingDiscoverer)
	if !ok {
		return parser.UnsupportedProviderFeatureError{
			Provider: p.Definition().Type,
			Feature:  "streaming discovery",
		}
	}
	return discoverer.DiscoverEach(ctx, yield)
}

func (p *omnigentParseCountingProvider) SourceForReconciliation(
	ctx context.Context, path, project string,
) (parser.SourceRef, bool, error) {
	resolver, ok := p.Provider.(parser.ReconciliationSourceResolver)
	if !ok {
		return parser.SourceRef{}, false, nil
	}
	return resolver.SourceForReconciliation(ctx, path, project)
}

func (p *omnigentParseCountingProvider) Parse(
	ctx context.Context, req parser.ParseRequest,
) (parser.ParseOutcome, error) {
	p.count.Add(1)
	if p.failOnce != nil && req.Source.DisplayPath == p.failPath &&
		p.failOnce.CompareAndSwap(false, true) {
		return parser.ParseOutcome{}, errors.New("injected omnigent member parse failure")
	}
	outcome, err := p.Provider.Parse(ctx, req)
	if p.results != nil {
		p.results.Add(int64(len(outcome.Results)))
	}
	return outcome, err
}

func omnigentDefaultProviderFactory(t *testing.T) parser.ProviderFactory {
	t.Helper()
	for _, factory := range parser.ProviderFactories() {
		if factory.Definition().Type == parser.AgentOmnigent {
			return factory
		}
	}
	require.FailNow(t, "Omnigent provider factory not registered")
	return nil
}

const omnigentSplitSyncDDL = `
CREATE TABLE conversations (
	workspace_id BIGINT NOT NULL DEFAULT 0, id VARCHAR(64),
	created_at INTEGER, updated_at INTEGER, title TEXT,
	parent_conversation_id VARCHAR(64), root_conversation_id VARCHAR(64),
	next_position INTEGER, PRIMARY KEY (workspace_id, id)
);
CREATE INDEX ix_conversations_updated_at
	ON conversations(workspace_id, updated_at, id);
CREATE TABLE omnigent_conversation_metadata (
	workspace_id BIGINT NOT NULL DEFAULT 0, id VARCHAR(64),
	kind SMALLINT, sub_agent_name VARCHAR(128), session_usage TEXT,
	workspace VARCHAR(2048), git_branch VARCHAR(255),
	PRIMARY KEY (workspace_id, id)
);
CREATE TABLE agent_configuration (
	workspace_id BIGINT NOT NULL DEFAULT 0, conversation_id VARCHAR(64),
	agent_id VARCHAR(64), reasoning_effort VARCHAR(32),
	model_override VARCHAR(128), harness_override VARCHAR(64),
	PRIMARY KEY (workspace_id, conversation_id)
);
CREATE TABLE conversation_items (
	workspace_id BIGINT NOT NULL DEFAULT 0,
	conversation_id VARCHAR(64) NOT NULL, id VARCHAR(64) NOT NULL,
	position INTEGER NOT NULL, type SMALLINT NOT NULL,
	data TEXT NOT NULL, search_text TEXT NOT NULL,
	PRIMARY KEY (workspace_id, conversation_id, id)
);
CREATE INDEX ix_conversation_items_conversation_id_position
	ON conversation_items(workspace_id, conversation_id, position);`

func writeOmnigentSplitSyncDB(t *testing.T, root string, count int) string {
	t.Helper()
	path := filepath.Join(root, "chat.db")
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = database.Exec(
		`CREATE TABLE alembic_version (version_num VARCHAR(32) NOT NULL)`,
	)
	require.NoError(t, err)
	for _, statement := range splitSQLStatements(omnigentSplitSyncDDL) {
		_, err = database.Exec(statement)
		require.NoError(t, err)
	}
	_, err = database.Exec(`INSERT INTO alembic_version VALUES ('split-sync-test')`)
	require.NoError(t, err)
	tx, err := database.Begin()
	require.NoError(t, err)
	for i := range count {
		id := fmt.Sprintf("conv_%04d", i)
		updatedAt := int64(1_700_000_000 + i)
		_, err = tx.Exec(`INSERT INTO conversations
			(workspace_id, id, created_at, updated_at, title, root_conversation_id)
			VALUES (0, ?, ?, ?, ?, ?)`,
			id, updatedAt-1, updatedAt, id, id)
		require.NoError(t, err)
		_, err = tx.Exec(`INSERT INTO omnigent_conversation_metadata
			(workspace_id, id, kind, workspace)
			VALUES (0, ?, 1, '/work/project')`, id)
		require.NoError(t, err)
		_, err = tx.Exec(`INSERT INTO conversation_items
			(workspace_id, conversation_id, id, position, type, data, search_text)
			VALUES (0, ?, ?, 0, 1, ?, 'initial')`, id, id+"_0",
			`{"role":"user","content":[{"type":"input_text","text":"initial"}]}`)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	require.NoError(t, database.Close())
	return path
}

// migrateOmnigentSplitSyncDBWorkspace rewrites a split-generation chat.db so
// its conversations move to a new workspace ID, changing their derived
// session key (and therefore their session ID) while the physical file path
// stays the same.
func migrateOmnigentSplitSyncDBWorkspace(
	t *testing.T, path string, workspaceID int64, conversationIDs ...string,
) {
	t.Helper()
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	_, err = database.Exec(`DELETE FROM conversation_items`)
	require.NoError(t, err)
	_, err = database.Exec(`DELETE FROM omnigent_conversation_metadata`)
	require.NoError(t, err)
	_, err = database.Exec(`DELETE FROM conversations`)
	require.NoError(t, err)
	for _, id := range conversationIDs {
		_, err = database.Exec(`INSERT INTO conversations
			(workspace_id, id, created_at, updated_at, title, root_conversation_id)
			VALUES (?, ?, 1, 2, 'migrated', ?)`, workspaceID, id, id)
		require.NoError(t, err)
		_, err = database.Exec(`INSERT INTO omnigent_conversation_metadata
			(workspace_id, id, kind, workspace)
			VALUES (?, ?, 1, '/work/project')`, workspaceID, id)
		require.NoError(t, err)
		_, err = database.Exec(`INSERT INTO conversation_items
			(workspace_id, conversation_id, id, position, type, data, search_text)
			VALUES (?, ?, ?, 0, 1, ?, 'migrated')`,
			workspaceID, id, id+"_migrated",
			`{"role":"user","content":[{"type":"input_text","text":"migrated"}]}`)
		require.NoError(t, err)
	}
}

// migrateOmnigentSyncDBToLegacyShape rewrites a split-generation chat.db in
// place into the single-table legacy shape (a VARCHAR kind column on
// conversations, no omnigent_conversation_metadata table), which
// detectOmnigentSchema reports as ErrOmnigentUnsupportedSchema. The physical
// file path stays the same, so sessions archived before the migration must
// survive the container becoming unsupported.
func migrateOmnigentSyncDBToLegacyShape(t *testing.T, path string) {
	t.Helper()
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	_, err = database.Exec(`DROP TABLE conversation_items`)
	require.NoError(t, err)
	_, err = database.Exec(`DROP TABLE omnigent_conversation_metadata`)
	require.NoError(t, err)
	_, err = database.Exec(`DROP TABLE agent_configuration`)
	require.NoError(t, err)
	_, err = database.Exec(`DROP TABLE conversations`)
	require.NoError(t, err)
	_, err = database.Exec(`CREATE TABLE conversations (
		id VARCHAR(64) PRIMARY KEY, created_at INTEGER, updated_at INTEGER,
		title TEXT, kind VARCHAR(16), root_conversation_id VARCHAR(64)
	)`)
	require.NoError(t, err)
	_, err = database.Exec(`CREATE TABLE conversation_items (
		id VARCHAR(64) PRIMARY KEY, conversation_id VARCHAR(64) NOT NULL,
		position INTEGER NOT NULL, type VARCHAR(32) NOT NULL,
		data TEXT NOT NULL, search_text TEXT NOT NULL
	)`)
	require.NoError(t, err)
	_, err = database.Exec(`INSERT INTO conversations
		(id, created_at, updated_at, title, kind, root_conversation_id)
		VALUES ('legacy', 1, 2, 'legacy', 'default', 'legacy')`)
	require.NoError(t, err)
	_, err = database.Exec(`INSERT INTO conversation_items
		(id, conversation_id, position, type, data, search_text)
		VALUES ('legacy_0', 'legacy', 0, 'message', ?, 'legacy')`,
		`{"role":"user","content":[{"type":"input_text","text":"legacy"}]}`)
	require.NoError(t, err)
}

func syncOmnigentArchive(
	t *testing.T, engine *sync.Engine, archive *db.DB, want int,
) {
	t.Helper()
	engine.SyncAll(context.Background(), nil)
	stats := engine.LastSyncStats()
	require.Zero(t, stats.Failed)
	page, err := archive.ListSessions(context.Background(), db.SessionFilter{
		Agent:           string(parser.AgentOmnigent),
		IncludeChildren: true,
		Limit:           1,
	})
	require.NoError(t, err)
	require.Equal(t, want, page.Total)
}

func splitSQLStatements(ddl string) []string {
	var statements []string
	start := 0
	for i, char := range ddl {
		if char != ';' {
			continue
		}
		if statement := ddl[start:i]; statement != "" {
			statements = append(statements, statement)
		}
		start = i + 1
	}
	return statements
}

func TestSyncOmnigentChangedPathWorkIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for _, archiveSize := range []int{130, 1030} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			root := t.TempDir()
			dbPath := writeOmnigentSplitSyncDB(t, root, archiveSize)
			changedID := fmt.Sprintf("conv_%04d", archiveSize/2)
			archive := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(archive, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentOmnigent: {root},
				},
				Machine: "local",
			})
			syncOmnigentArchive(t, engine, archive, archiveSize)
			engine.SyncAll(context.Background(), nil)
			assert.Zero(t, engine.LastSyncStats().Synced,
				"unchanged full sync should not rewrite member sessions")

			writer, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			changedAt := time.Now().Unix()
			_, err = writer.Exec(
				`UPDATE conversations SET updated_at = ?
				 WHERE workspace_id = 0 AND id = ?`,
				changedAt, changedID)
			require.NoError(t, err)
			_, err = writer.Exec(`INSERT INTO conversation_items
				(workspace_id, conversation_id, id, position, type, data, search_text)
				VALUES (0, ?, ?, 1, 1, ?, 'changed')`, changedID, changedID+"_1",
				`{"role":"assistant","content":[{"type":"output_text","text":"changed"}]}`)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			engine.SyncPaths([]string{dbPath + "-wal"})
			assert.Equal(t, 1, engine.LastSyncStats().Synced,
				"one changed conversation should produce one archive write")
			changed, err := archive.GetSessionFull(
				context.Background(), "omnigent:0:"+changedID)
			require.NoError(t, err)
			require.NotNil(t, changed)
			assert.Equal(t, 2, changed.MessageCount)
			require.NotNil(t, changed.FileMtime)
			assert.Equal(t, changedAt*1_000_000_000, *changed.FileMtime)

			unchangedID := "conv_0001"
			if changedID == unchangedID {
				unchangedID = "conv_0000"
			}
			unchanged, err := archive.GetSession(
				context.Background(), "omnigent:0:"+unchangedID)
			require.NoError(t, err)
			require.NotNil(t, unchanged)
			assert.Equal(t, 1, unchanged.MessageCount)
			engine.SyncAll(context.Background(), nil)
			assert.Zero(t, engine.LastSyncStats().Synced,
				"member sync followed by unchanged full sync should not rewrite")

			writer, err = sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			tx, err := writer.Begin()
			require.NoError(t, err)
			_, err = tx.Exec(
				`DELETE FROM conversation_items
				  WHERE workspace_id = 0 AND conversation_id = 'conv_0001'`)
			require.NoError(t, err)
			_, err = tx.Exec(
				`DELETE FROM omnigent_conversation_metadata
				  WHERE workspace_id = 0 AND id = 'conv_0001'`)
			require.NoError(t, err)
			_, err = tx.Exec(
				`DELETE FROM conversations WHERE workspace_id = 0 AND id = 'conv_0001'`)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO conversations
				(workspace_id, id, created_at, updated_at, title, root_conversation_id)
				VALUES (0, 'replacement', 1, ?, 'replacement', 'replacement')`,
				time.Now().Unix())
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO omnigent_conversation_metadata
				(workspace_id, id, kind, workspace)
				VALUES (0, 'replacement', 1, '/work/project')`)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO conversation_items
				(workspace_id, conversation_id, id, position, type, data, search_text)
				VALUES (0, 'replacement', 'replacement_0', 0, 1, ?, 'replacement')`,
				`{"role":"user","content":[{"type":"input_text","text":"replacement"}]}`)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			require.NoError(t, writer.Close())

			engine.SyncPaths([]string{dbPath})
			deleted, err := archive.GetSession(
				context.Background(), "omnigent:0:conv_0001")
			require.NoError(t, err)
			assert.NotNil(t, deleted,
				"changed-path work must defer archive-wide deletion proof")
			replacement, err := archive.GetSession(
				context.Background(), "omnigent:0:replacement")
			require.NoError(t, err)
			require.NotNil(t, replacement,
				"one changed-path pass must sync the replacement conversation")
		})
	}
}

func TestSyncOmnigentUnchangedAfterBoundedInitializationDoesNoWork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	writeOmnigentSplitSyncDB(t, root, 200)
	archive := dbtest.OpenTestDB(t)
	var parseCount atomic.Int64
	factory := omnigentParseCountingFactory{
		delegate: omnigentDefaultProviderFactory(t),
		count:    &parseCount,
	}
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine:           "local",
		ProviderFactories: []parser.ProviderFactory{factory},
	})
	syncOmnigentArchive(t, engine, archive, 200)

	parseCount.Store(0)
	engine.SyncAll(context.Background(), nil)
	assert.Zero(t, parseCount.Load(),
		"unchanged container must not reparse every conversation")
}

func TestSyncOmnigentInitialContainerFailureIsRetried(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 3)
	archive := dbtest.OpenTestDB(t)
	var failed atomic.Bool
	factory := omnigentParseCountingFactory{
		delegate: omnigentDefaultProviderFactory(t),
		count:    &atomic.Int64{},
		failPath: dbPath,
		failOnce: &failed,
	}
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine:           "local",
		ProviderFactories: []parser.ProviderFactory{factory},
	})
	defer engine.Close()
	engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 1, engine.LastSyncStats().Failed)

	engine.SyncAll(context.Background(), nil)
	session, err := archive.GetSession(context.Background(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	assert.NotNil(t, session, "the failed physical source must remain retryable")
}

func TestSyncOmnigentFailedMemberIsReplayedByScheduledContainerSync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 1)
	archive := dbtest.OpenTestDB(t)
	var failed atomic.Bool
	factory := omnigentParseCountingFactory{
		delegate: omnigentDefaultProviderFactory(t),
		count:    &atomic.Int64{},
		failPath: parser.VirtualSourcePath(dbPath, "0:conv_0000"),
		failOnce: &failed,
	}
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine:           "local",
		ProviderFactories: []parser.ProviderFactory{factory},
	})
	t.Cleanup(engine.Close)
	syncOmnigentArchive(t, engine, archive, 1)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE conversations SET updated_at = ?
		 WHERE workspace_id = 0 AND id = 'conv_0000'`,
		time.Now().Unix(),
	)
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO conversation_items
		(workspace_id, conversation_id, id, position, type, data, search_text)
		VALUES (0, 'conv_0000', 'conv_0000_1', 1, 1, ?, 'second')`,
		`{"role":"assistant","content":[{"type":"output_text","text":"second"}]}`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	err = engine.SyncPathsContext(t.Context(), []string{dbPath})
	require.Error(t, err)
	assert.Equal(t, 1, engine.LastSyncStats().Failed)
	require.True(t, failed.Load(), "the changed virtual member must fail once")

	stats := engine.SyncAll(t.Context(), nil)
	assert.Zero(t, stats.Failed)
	updated, err := archive.GetSessionFull(t.Context(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, 2, updated.MessageCount,
		"the scheduled container pass must replay the failed member")
}

func TestSyncAllSinceOmnigentCutoffDefersStaleMemberUntilFullSync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 1)
	archive := dbtest.OpenTestDB(t)
	var failed atomic.Bool
	factory := omnigentParseCountingFactory{
		delegate: omnigentDefaultProviderFactory(t),
		count:    &atomic.Int64{},
		failPath: parser.VirtualSourcePath(dbPath, "0:conv_0000"),
		failOnce: &failed,
	}
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine:           "local",
		ProviderFactories: []parser.ProviderFactory{factory},
	})
	t.Cleanup(engine.Close)
	syncOmnigentArchive(t, engine, archive, 1)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE conversations SET updated_at = ?
		 WHERE workspace_id = 0 AND id = 'conv_0000'`,
		time.Now().Unix(),
	)
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO conversation_items
		(workspace_id, conversation_id, id, position, type, data, search_text)
		VALUES (0, 'conv_0000', 'conv_0000_1', 1, 1, ?, 'second')`,
		`{"role":"assistant","content":[{"type":"output_text","text":"second"}]}`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	require.Error(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
	require.True(t, failed.Load(), "the changed virtual member must fail once")

	stats := engine.SyncAllSince(
		t.Context(), time.Now().Add(time.Hour), nil,
	)
	require.Zero(t, stats.Failed)
	deferred, err := archive.GetSessionFull(t.Context(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, deferred)
	assert.Equal(t, 1, deferred.MessageCount,
		"a cutoff newer than the container may defer the stale member")

	stats = engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	updated, err := archive.GetSessionFull(t.Context(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, 2, updated.MessageCount,
		"the next full container pass must repair the stale member")
}

func TestOmnigentStaleMemberSurvivesUnrelatedScopedSync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for _, tc := range []struct {
		name string
		sync func(*sync.Engine, string) error
	}{
		{
			name: "changed path",
			sync: func(engine *sync.Engine, dbPath string) error {
				return engine.SyncPathsContext(t.Context(), []string{dbPath})
			},
		},
		{
			name: "root scoped",
			sync: func(engine *sync.Engine, dbPath string) error {
				stats := engine.SyncRootsSince(
					t.Context(), []string{filepath.Dir(dbPath)}, time.Time{}, nil,
				)
				require.Zero(t, stats.Failed)
				return nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rootA := t.TempDir()
			rootB := t.TempDir()
			dbA := writeOmnigentSplitSyncDB(t, rootA, 1)
			dbB := writeOmnigentSplitSyncDB(t, rootB, 1)
			setOmnigentSyncWorkspace(t, dbB, 1)
			archive := dbtest.OpenTestDB(t)
			var failed atomic.Bool
			factory := omnigentParseCountingFactory{
				delegate: omnigentDefaultProviderFactory(t),
				count:    &atomic.Int64{},
				failPath: parser.VirtualSourcePath(dbA, "0:conv_0000"),
				failOnce: &failed,
			}
			engine := sync.NewEngine(archive, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentOmnigent: {rootA, rootB},
				},
				Machine:           "local",
				ProviderFactories: []parser.ProviderFactory{factory},
			})
			t.Cleanup(engine.Close)
			syncOmnigentArchive(t, engine, archive, 2)

			appendOmnigentSyncMessage(t, dbA, "missed")
			require.Error(t,
				engine.SyncPathsContext(t.Context(), []string{dbA}))
			require.True(t, failed.Load())

			require.NoError(t, tc.sync(engine, dbB))
			stale, err := archive.GetSessionFull(
				t.Context(), "omnigent:0:conv_0000",
			)
			require.NoError(t, err)
			require.NotNil(t, stale)
			assert.Equal(t, 1, stale.MessageCount)

			stats := engine.SyncAll(t.Context(), nil)
			require.Zero(t, stats.Failed)
			repaired, err := archive.GetSessionFull(
				t.Context(), "omnigent:0:conv_0000",
			)
			require.NoError(t, err)
			require.NotNil(t, repaired)
			assert.Equal(t, 2, repaired.MessageCount,
				"the next full container pass must repair root A's stale member")
		})
	}
}

func appendOmnigentSyncMessage(t *testing.T, dbPath, text string) {
	t.Helper()
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE conversations SET updated_at = ?
		 WHERE workspace_id = 0 AND id = 'conv_0000'`,
		time.Now().Unix(),
	)
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO conversation_items
		(workspace_id, conversation_id, id, position, type, data, search_text)
		VALUES (0, 'conv_0000', ?, 1, 1, ?, ?)`,
		"conv_0000_"+text,
		fmt.Sprintf(
			`{"role":"assistant","content":[{"type":"output_text","text":%q}]}`,
			text,
		),
		text,
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
}

func setOmnigentSyncWorkspace(t *testing.T, dbPath string, workspaceID int64) {
	t.Helper()
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	for _, table := range []string{
		"conversation_items",
		"omnigent_conversation_metadata",
		"conversations",
	} {
		_, err = writer.Exec(
			`UPDATE `+table+` SET workspace_id = ? WHERE workspace_id = 0`,
			workspaceID,
		)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
}

func TestSyncOmnigentFullSyncWritesOnlyChangedMembers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for _, archiveSize := range []int{130, 1030} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			root := t.TempDir()
			dbPath := writeOmnigentSplitSyncDB(t, root, archiveSize)
			archive := dbtest.OpenTestDB(t)
			var parseCount, resultCount atomic.Int64
			factory := omnigentParseCountingFactory{
				delegate: omnigentDefaultProviderFactory(t),
				count:    &parseCount,
				results:  &resultCount,
			}
			engine := sync.NewEngine(archive, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentOmnigent: {root},
				},
				Machine:           "local",
				ProviderFactories: []parser.ProviderFactory{factory},
			})
			syncOmnigentArchive(t, engine, archive, archiveSize)

			parseCount.Store(0)
			resultCount.Store(0)
			engine.SyncAll(context.Background(), nil)
			assert.Zero(t, parseCount.Load(),
				"an unchanged container must be skipped without parsing")
			assert.Zero(t, resultCount.Load(),
				"an unchanged container must emit no results")

			writer, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			_, err = writer.Exec(`UPDATE conversations
				SET updated_at = ? WHERE workspace_id = 0 AND id = 'conv_0000'`,
				time.Now().Unix())
			require.NoError(t, err)
			_, err = writer.Exec(`INSERT INTO conversation_items
				(workspace_id, conversation_id, id, position, type, data, search_text)
				VALUES (0, 'conv_0000', 'changed', 1, 1, ?, 'changed')`,
				`{"role":"assistant","content":[{"type":"output_text","text":"changed"}]}`)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			parseCount.Store(0)
			resultCount.Store(0)
			engine.SyncAll(context.Background(), nil)
			assert.Equal(t, 1, engine.LastSyncStats().Synced,
				"only the changed member may be rewritten")
			assert.Equal(t, int64(1), parseCount.Load(),
				"a changed container must be parsed once as a whole")
			assert.Equal(t, int64(archiveSize), resultCount.Load(),
				"a whole-container parse emits every member for unchanged dedup")
		})
	}
}

func TestSyncOmnigentRestartCacheWarmsBoundedChangeTracker(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	var watcherResultCounts []int64
	for _, archiveSize := range []int{130, 1030} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			root := t.TempDir()
			dbPath := writeOmnigentSplitSyncDB(t, root, archiveSize)
			archive := dbtest.OpenTestDB(t)

			firstEngine := sync.NewEngine(archive, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentOmnigent: {root},
				},
				Machine: "local",
			})
			syncOmnigentArchive(t, firstEngine, archive, archiveSize)
			firstEngine.Close()

			var parseCount, resultCount atomic.Int64
			restartedFactory := omnigentParseCountingFactory{
				delegate: omnigentDefaultProviderFactory(t),
				count:    &parseCount,
				results:  &resultCount,
			}
			restarted := sync.NewEngine(archive, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentOmnigent: {root},
				},
				Machine:           "local",
				ProviderFactories: []parser.ProviderFactory{restartedFactory},
			})
			t.Cleanup(restarted.Close)

			restarted.SyncAll(t.Context(), nil)
			require.Zero(t, restarted.LastSyncStats().Failed)
			assert.Zero(t, parseCount.Load(),
				"restart validation should reuse the persisted container cache")

			appendOmnigentSyncMessage(t, dbPath, "after_restart")
			parseCount.Store(0)
			resultCount.Store(0)
			require.NoError(t,
				restarted.SyncPathsContext(t.Context(), []string{dbPath}))
			require.Zero(t, restarted.LastSyncStats().Failed)
			assert.Equal(t, parseCount.Load(), resultCount.Load())
			assert.Equal(t, int64(1), resultCount.Load(),
				"the restart-warmed tracker must replay only the changed member")
			watcherResultCounts = append(
				watcherResultCounts, resultCount.Load(),
			)
		})
	}
	require.Len(t, watcherResultCounts, 2)
	assert.Equal(t, watcherResultCounts[0], watcherResultCounts[1],
		"first watcher work after restart must not grow with archive size")
}

func TestResyncOmnigentForcesCompleteDiscovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	writeOmnigentSplitSyncDB(t, root, 3)
	archive := dbtest.OpenTestDB(t)
	var parseCount, resultCount atomic.Int64
	factory := omnigentParseCountingFactory{
		delegate: omnigentDefaultProviderFactory(t),
		count:    &parseCount,
		results:  &resultCount,
	}
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine:           "local",
		ProviderFactories: []parser.ProviderFactory{factory},
	})
	engine.SyncAll(context.Background(), nil)

	parseCount.Store(0)
	resultCount.Store(0)
	stats := engine.ResyncAll(context.Background(), nil)
	assert.False(t, stats.Aborted)
	assert.Equal(t, 3, stats.Synced)
	assert.Equal(t, int64(1), parseCount.Load())
	assert.Equal(t, int64(3), resultCount.Load(),
		"archive rebuild must bypass incremental discovery")
}

func TestSyncOmnigentCompleteContainerMissingConversationPreservesArchive(
	t *testing.T,
) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 2)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	syncOmnigentArchive(t, engine, archive, 2)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`DELETE FROM conversation_items
		  WHERE workspace_id = 0 AND conversation_id = 'conv_0001'`,
	)
	require.NoError(t, err)
	_, err = writer.Exec(
		`DELETE FROM omnigent_conversation_metadata
		  WHERE workspace_id = 0 AND id = 'conv_0001'`,
	)
	require.NoError(t, err)
	_, err = writer.Exec(
		`DELETE FROM conversations WHERE workspace_id = 0 AND id = 'conv_0001'`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	active, err := archive.GetSession(t.Context(), "omnigent:0:conv_0001")
	require.NoError(t, err)
	assert.NotNil(t, active)
	archived, err := archive.GetSessionFull(t.Context(), "omnigent:0:conv_0001")
	require.NoError(t, err)
	assertSourceMissingState(t, archived)
	assert.Equal(t, 1, archived.MessageCount)
}

func TestSyncOmnigentCompleteEmptyContainerPreservesFinalConversation(
	t *testing.T,
) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 1)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	syncOmnigentArchive(t, engine, archive, 1)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`DELETE FROM conversation_items`)
	require.NoError(t, err)
	_, err = writer.Exec(`DELETE FROM omnigent_conversation_metadata`)
	require.NoError(t, err)
	_, err = writer.Exec(`DELETE FROM conversations`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	active, err := archive.GetSession(t.Context(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	assert.NotNil(t, active)
	archived, err := archive.GetSessionFull(t.Context(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	assertSourceMissingState(t, archived)
	assert.Equal(t, 1, archived.MessageCount)
}

func TestSyncOmnigentDataVersionFailurePreventsContainerCache(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	claudeRoot := t.TempDir()
	writeOmnigentSplitSyncDB(t, root, 2)
	archive := dbtest.OpenTestDB(t)
	raw, err := sql.Open("sqlite3", archive.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	_, err = raw.Exec(`CREATE TRIGGER fail_omnigent_data_version
		BEFORE UPDATE OF data_version ON sessions
		WHEN NEW.id = 'omnigent:0:conv_0000'
		BEGIN
			SELECT RAISE(FAIL, 'injected data-version failure');
		END`)
	require.NoError(t, err)

	var parseCount atomic.Int64
	factory := omnigentParseCountingFactory{
		delegate: omnigentDefaultProviderFactory(t),
		count:    &parseCount,
	}
	claudeFactory, ok := parser.ProviderFactoryByType(parser.AgentClaude)
	require.True(t, ok)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
			parser.AgentClaude:   {claudeRoot},
		},
		Machine:           "local",
		ProviderFactories: []parser.ProviderFactory{factory, claudeFactory},
	})
	engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 1, engine.LastSyncStats().Failed)
	assert.Less(t, archive.GetSessionDataVersion("omnigent:0:conv_0000"),
		db.CurrentDataVersion())

	_, err = raw.Exec(`DROP TRIGGER fail_omnigent_data_version`)
	require.NoError(t, err)
	claudePath := filepath.Join(claudeRoot, "project", "unrelated.jsonl")
	dbtest.WriteTestFile(t, claudePath, []byte(
		testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", "unrelated").
			String(),
	))
	engine.SyncPaths([]string{claudePath})
	require.Zero(t, engine.LastSyncStats().Failed,
		"the unrelated watcher pass must complete successfully")

	parseCount.Store(0)
	engine.SyncAll(context.Background(), nil)
	assert.Equal(t, int64(1), parseCount.Load(),
		"stale virtual member must bypass the container cache")
	assert.Equal(t, db.CurrentDataVersion(),
		archive.GetSessionDataVersion("omnigent:0:conv_0000"))
}

func TestSyncOmnigentFailedCurrentUpdateForcesContentReplacement(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 1)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	defer engine.Close()
	engine.SyncAll(context.Background(), nil)
	require.Equal(t, db.CurrentDataVersion(),
		archive.GetSessionDataVersion("omnigent:0:conv_0000"))

	raw, err := sql.Open("sqlite3", archive.Path())
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.Exec(`CREATE TRIGGER fail_omnigent_message_append
		BEFORE INSERT ON messages
		WHEN NEW.session_id = 'omnigent:0:conv_0000' AND NEW.ordinal = 1
		BEGIN
			SELECT RAISE(FAIL, 'injected message append failure');
		END`)
	require.NoError(t, err)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`UPDATE conversations SET updated_at = ?
		WHERE workspace_id = 0 AND id = 'conv_0000'`, time.Now().Unix())
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO conversation_items
		(workspace_id, conversation_id, id, position, type, data, search_text)
		VALUES (0, 'conv_0000', 'conv_0000_1', 1, 1, ?, 'second')`,
		`{"role":"assistant","content":[{"type":"output_text","text":"second"}]}`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 1, engine.LastSyncStats().Failed)
	assert.Less(t, archive.GetSessionDataVersion("omnigent:0:conv_0000"),
		db.CurrentDataVersion(),
		"an incomplete current-session update must persist retry state")

	_, err = raw.Exec(`DROP TRIGGER fail_omnigent_message_append`)
	require.NoError(t, err)
	engine.SyncAll(context.Background(), nil)
	messages, err := archive.GetMessages(
		context.Background(), "omnigent:0:conv_0000", 0, 10, true,
	)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "second", messages[1].Content)
}

// TestSyncSingleSessionOmnigentFailedWriteDemotesDataVersion covers the
// single-session resync path: when the member's write fails partway (the
// session row is updated but message replacement is not), the stored data
// version must be demoted so the next container parse repairs the member
// instead of comparing it as unchanged. Shared-container members have no
// per-file mtime to invalidate, so the demotion is the only retry signal.
func TestSyncSingleSessionOmnigentFailedWriteDemotesDataVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 1)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	defer engine.Close()
	engine.SyncAll(context.Background(), nil)
	require.Equal(t, db.CurrentDataVersion(),
		archive.GetSessionDataVersion("omnigent:0:conv_0000"))

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`UPDATE conversations SET updated_at = ?
		WHERE workspace_id = 0 AND id = 'conv_0000'`, time.Now().Unix())
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO conversation_items
		(workspace_id, conversation_id, id, position, type, data, search_text)
		VALUES (0, 'conv_0000', 'conv_0000_1', 1, 1, ?, 'second')`,
		`{"role":"assistant","content":[{"type":"output_text","text":"second"}]}`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	raw, err := sql.Open("sqlite3", archive.Path())
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.Exec(`CREATE TRIGGER fail_omnigent_message_append
		BEFORE INSERT ON messages
		WHEN NEW.session_id = 'omnigent:0:conv_0000' AND NEW.ordinal = 1
		BEGIN
			SELECT RAISE(FAIL, 'injected message append failure');
		END`)
	require.NoError(t, err)

	require.Error(t, engine.SyncSingleSession("omnigent:0:conv_0000"),
		"the injected write failure must surface to the resync caller")
	assert.Less(t, archive.GetSessionDataVersion("omnigent:0:conv_0000"),
		db.CurrentDataVersion(),
		"a failed single-session write must demote the data version")

	_, err = raw.Exec(`DROP TRIGGER fail_omnigent_message_append`)
	require.NoError(t, err)
	engine.SyncAll(context.Background(), nil)
	assert.Equal(t, db.CurrentDataVersion(),
		archive.GetSessionDataVersion("omnigent:0:conv_0000"))
	messages, err := archive.GetMessages(
		context.Background(), "omnigent:0:conv_0000", 0, 10, true,
	)
	require.NoError(t, err)
	require.Len(t, messages, 2,
		"the demoted member must be repaired by the next container pass")
	assert.Equal(t, "second", messages[1].Content)
}

func TestSyncOmnigentPersistsJSONStringToolResult(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 1)
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO conversation_items
		(workspace_id, conversation_id, id, position, type, data, search_text) VALUES
		(0, 'conv_0000', 'call', 1, 2,
		 '{"call_id":"call-json","name":"inspect","arguments":"{}"}', ''),
		(0, 'conv_0000', 'result', 2, 3,
		 '{"call_id":"call-json","output":"{\"ok\":true}"}', '')`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	engine.SyncAll(context.Background(), nil)

	messages := fetchMessages(t, archive, "omnigent:0:conv_0000")
	require.Len(t, messages, 2)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, `{"ok":true}`, messages[1].ToolCalls[0].ResultContent)
	assert.Equal(t, len(`{"ok":true}`),
		messages[1].ToolCalls[0].ResultContentLength)
}

func TestSyncOmnigentFallbackUsageAppearsInAnalytics(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 1)
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO agent_configuration
		(workspace_id, conversation_id, model_override)
		VALUES (0, 'conv_0000', 'claude-sonnet')`)
	require.NoError(t, err)
	_, err = writer.Exec(`UPDATE omnigent_conversation_metadata
		SET session_usage =
		    '{"input_tokens":120,"output_tokens":30,"total_cost_usd":0.25}'
		WHERE workspace_id = 0 AND id = 'conv_0000'`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	engine.SyncAll(context.Background(), nil)

	events, err := archive.GetUsageEvents(context.Background(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "claude-sonnet", events[0].Model)
	daily, err := archive.GetDailyUsage(context.Background(), db.UsageFilter{
		From: "2023-11-01", To: "2023-11-30",
	})
	require.NoError(t, err)
	require.Len(t, daily.Daily, 1)
	assert.Equal(t, 120, daily.Daily[0].InputTokens)
	assert.Equal(t, 30, daily.Daily[0].OutputTokens)
	assert.Equal(t, money.Money{Microdollars: 250_000}, daily.Daily[0].TotalCost)
}

func TestSyncOmnigentInPlaceEditIsReconciledByFullSync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 1)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	engine.SyncAll(context.Background(), nil)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`UPDATE conversation_items
		SET data = ?, search_text = 'edited'
		WHERE workspace_id = 0 AND conversation_id = 'conv_0000'
		  AND id = 'conv_0000_0'`,
		`{"role":"user","content":[{"type":"input_text","text":"edited"}]}`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	// An in-place edit to an existing row inserts no new rowid and advances
	// neither the item nor the conversation high-water mark, so it is
	// invisible to the changed-member scan: the event stays bounded by the
	// changed set and defers the edit instead of probing every member.
	engine.SyncPaths([]string{dbPath})
	deferred, err := archive.GetAllMessages(
		context.Background(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	require.Len(t, deferred, 1)
	assert.Equal(t, "initial", deferred[0].Content,
		"the changed-path scan must defer an edit it cannot see")

	engine = sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	engine.SyncAll(context.Background(), nil)
	updated, err := archive.GetAllMessages(
		context.Background(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	require.Len(t, updated, 1)
	assert.Equal(t, "edited", updated[0].Content,
		"the scheduled full sync must reconcile edits the scan deferred")
}

func TestSyncOmnigentArchiveAuditDetectsInPlaceItemEdit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 1)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	engine.SyncAll(context.Background(), nil)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`UPDATE conversation_items
		SET data = ?, search_text = 'edited'
		WHERE workspace_id = 0 AND conversation_id = 'conv_0000'
		  AND id = 'conv_0000_0'`,
		`{"role":"user","content":[{"type":"input_text","text":"edited"}]}`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	require.NoError(t, engine.ReconcileWatchRoots(
		t.Context(), []string{root}, false,
	))
	messages, err := archive.GetAllMessages(
		context.Background(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "edited", messages[0].Content)
}

func TestAuditOmnigentDetectsMultiWorkspaceMetadataOnlyEdit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 128)
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO conversations
		(workspace_id, id, created_at, updated_at, title, root_conversation_id)
		VALUES (7, 'conv_workspace', 1, 2, 'before', 'conv_workspace')`)
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO omnigent_conversation_metadata
		(workspace_id, id, kind, workspace)
		VALUES (7, 'conv_workspace', 1, '/work/before')`)
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO conversation_items
		(workspace_id, conversation_id, id, position, type, data, search_text)
		VALUES (7, 'conv_workspace', 'workspace_item', 0, 1, ?, 'initial')`,
		`{"role":"user","content":[{"type":"input_text","text":"initial"}]}`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	syncOmnigentArchive(t, engine, archive, 129)

	writer, err = sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`UPDATE omnigent_conversation_metadata
		SET workspace = '/work/after'
		WHERE workspace_id = 7 AND id = 'conv_workspace'`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine.SyncPaths([]string{dbPath})
	deferred, err := archive.GetSession(
		t.Context(), "omnigent:7:conv_workspace",
	)
	require.NoError(t, err)
	require.NotNil(t, deferred)
	assert.Equal(t, "/work/before", deferred.Cwd,
		"bounded watcher discovery may defer a metadata-only edit")

	require.NoError(t, engine.ReconcileWatchRoots(
		t.Context(), []string{root}, false,
	))
	reconciled, err := archive.GetSession(
		t.Context(), "omnigent:7:conv_workspace",
	)
	require.NoError(t, err)
	require.NotNil(t, reconciled)
	assert.Equal(t, "/work/after", reconciled.Cwd,
		"authoritative reconciliation must refresh multi-workspace metadata")
}

func TestScheduledOmnigentReconciliationCatchesMetadataOnlyUsageEdit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	const targetID = "omnigent:0:conv_0000"
	boundedParseCounts := make(map[int]int64)
	for _, archiveSize := range []int{130, 1030} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			root := t.TempDir()
			dbPath := writeOmnigentSplitSyncDB(t, root, archiveSize)
			archive := dbtest.OpenTestDB(t)
			var parseCount atomic.Int64
			factory := omnigentParseCountingFactory{
				delegate: omnigentDefaultProviderFactory(t),
				count:    &parseCount,
			}
			engine := sync.NewEngine(archive, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentOmnigent: {root},
				},
				Machine:           "local",
				ProviderFactories: []parser.ProviderFactory{factory},
			})
			t.Cleanup(engine.Close)
			syncOmnigentArchive(t, engine, archive, archiveSize)

			writer, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			_, err = writer.Exec(`
				UPDATE omnigent_conversation_metadata
				   SET session_usage = ?
				 WHERE workspace_id = 0 AND id = 'conv_0000'`,
				`{"input_tokens":321,"output_tokens":45}`,
			)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			engine.SyncPaths([]string{dbPath})
			deferred, err := archive.GetUsageEvents(t.Context(), targetID)
			require.NoError(t, err)
			assert.Empty(t, deferred,
				"the bounded watcher scan may defer a metadata-only edit")

			parseCount.Store(0)
			require.NoError(t, engine.ReconcileProviderRoots(
				t.Context(), parser.AgentOmnigent, []string{root},
			))
			assert.Equal(t, int64(1), parseCount.Load(),
				"a metadata-only edit must cost one whole-container parse")
			events, err := archive.GetUsageEvents(t.Context(), targetID)
			require.NoError(t, err)
			require.Len(t, events, 1)
			assert.Equal(t, 321, events[0].InputTokens)
			assert.Equal(t, 45, events[0].OutputTokens)
			boundedParseCounts[archiveSize] = parseCount.Load()

			parseCount.Store(0)
			require.NoError(t, engine.ReconcileProviderRoots(
				t.Context(), parser.AgentOmnigent, []string{root},
			))
			assert.Zero(t, parseCount.Load(),
				"an unchanged container must cost the next scheduled pass nothing")
		})
	}
	assert.Equal(t, boundedParseCounts[130], boundedParseCounts[1030],
		"scheduled metadata work must not grow with the conversation archive")
}

func TestSyncPathsOmnigentRootMetadataRefreshesExistingSubagent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	observed := make(map[int]int64)
	for _, archiveSize := range []int{130, 1030} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			root := t.TempDir()
			dbPath := writeOmnigentSplitSyncDB(t, root, archiveSize)
			writer, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			_, err = writer.Exec(`
				UPDATE conversations
				   SET parent_conversation_id = 'conv_0000',
				       root_conversation_id = 'conv_0000'
				 WHERE workspace_id = 0 AND id = 'conv_0001'`)
			require.NoError(t, err)
			_, err = writer.Exec(`
				UPDATE omnigent_conversation_metadata
				   SET workspace = '/work/before', git_branch = 'main'
				 WHERE workspace_id = 0 AND id = 'conv_0000'`)
			require.NoError(t, err)
			_, err = writer.Exec(`
				UPDATE omnigent_conversation_metadata
				   SET kind = 2, workspace = '', git_branch = ''
				 WHERE workspace_id = 0 AND id = 'conv_0001'`)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			archive := dbtest.OpenTestDB(t)
			var resultCount atomic.Int64
			factory := omnigentParseCountingFactory{
				delegate: omnigentDefaultProviderFactory(t),
				count:    new(atomic.Int64),
				results:  &resultCount,
			}
			engine := sync.NewEngine(archive, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentOmnigent: {root},
				},
				Machine:           "local",
				ProviderFactories: []parser.ProviderFactory{factory},
			})
			t.Cleanup(engine.Close)
			syncOmnigentArchive(t, engine, archive, archiveSize)
			childID := "omnigent:0:conv_0001"
			before, err := archive.GetSession(t.Context(), childID)
			require.NoError(t, err)
			require.NotNil(t, before)
			assert.Equal(t, "/work/before", before.Cwd)
			assert.Equal(t, "before", before.Project)
			assert.Equal(t, "main", before.GitBranch)

			writer, err = sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			_, err = writer.Exec(`
				UPDATE conversations
				   SET updated_at = ?
				 WHERE workspace_id = 0 AND id = 'conv_0000'`,
				time.Now().Unix(),
			)
			require.NoError(t, err)
			_, err = writer.Exec(`
				UPDATE omnigent_conversation_metadata
				   SET workspace = '/work/after', git_branch = 'review'
				 WHERE workspace_id = 0 AND id = 'conv_0000'`)
			require.NoError(t, err)
			_, err = writer.Exec(`INSERT INTO conversation_items
				(workspace_id, conversation_id, id, position, type, data, search_text)
				VALUES (0, 'conv_0000', 'conv_0000_refresh', 1, 1, ?, 'refresh')`,
				`{"role":"assistant","content":[{"type":"output_text","text":"refresh"}]}`)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			resultCount.Store(0)
			require.NoError(t, engine.SyncPathsContext(
				t.Context(), []string{dbPath},
			))
			observed[archiveSize] = resultCount.Load()
			after, err := archive.GetSession(t.Context(), childID)
			require.NoError(t, err)
			require.NotNil(t, after)
			assert.Equal(t, "/work/after", after.Cwd)
			assert.Equal(t, "after", after.Project)
			assert.Equal(t, "review", after.GitBranch)
		})
	}
	assert.Equal(t, observed[130], observed[1030],
		"dependent metadata refresh work must not grow with unrelated conversations")
}

func TestScheduledOmnigentReconciliationIsBoundedByChangedMembers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	observed := make(map[int]int64)
	for _, archiveSize := range []int{130, 1030} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			root := t.TempDir()
			dbPath := writeOmnigentSplitSyncDB(t, root, archiveSize)
			archive := dbtest.OpenTestDB(t)
			var parseCount atomic.Int64
			factory := omnigentParseCountingFactory{
				delegate: omnigentDefaultProviderFactory(t),
				count:    &parseCount,
			}
			engine := sync.NewEngine(archive, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentOmnigent: {root},
				},
				Machine:           "local",
				ProviderFactories: []parser.ProviderFactory{factory},
			})
			t.Cleanup(engine.Close)
			syncOmnigentArchive(t, engine, archive, archiveSize)

			parseCount.Store(0)
			require.NoError(t, engine.ReconcileProviderRoots(
				t.Context(), parser.AgentOmnigent, []string{root},
			))
			assert.Zero(t, parseCount.Load(),
				"an unchanged container must cost a scheduled pass zero parses")

			changedID := fmt.Sprintf("conv_%04d", archiveSize/2)
			writer, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			_, err = writer.Exec(
				`UPDATE conversations SET updated_at = ?
				 WHERE workspace_id = 0 AND id = ?`,
				time.Now().Unix(), changedID,
			)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			parseCount.Store(0)
			require.NoError(t, engine.ReconcileProviderRoots(
				t.Context(), parser.AgentOmnigent, []string{root},
			))
			observed[archiveSize] = parseCount.Load()
			assert.Equal(t, int64(1), parseCount.Load(),
				"a changed container must cost one whole-container parse")

			deletedID := fmt.Sprintf("conv_%04d", archiveSize-1)
			writer, err = sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			_, err = writer.Exec(
				`DELETE FROM conversation_items
				  WHERE workspace_id = 0 AND conversation_id = ?`,
				deletedID,
			)
			require.NoError(t, err)
			_, err = writer.Exec(
				`DELETE FROM omnigent_conversation_metadata
				  WHERE workspace_id = 0 AND id = ?`, deletedID,
			)
			require.NoError(t, err)
			_, err = writer.Exec(
				`DELETE FROM conversations WHERE workspace_id = 0 AND id = ?`,
				deletedID,
			)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			require.NoError(t, engine.ReconcileProviderRoots(
				t.Context(), parser.AgentOmnigent, []string{root},
			))
			active, err := archive.GetSession(
				t.Context(), "omnigent:0:"+deletedID,
			)
			require.NoError(t, err)
			assert.NotNil(t, active,
				"the deleted member must remain browsable")
			archived, err := archive.GetSessionFull(
				t.Context(), "omnigent:0:"+deletedID,
			)
			require.NoError(t, err)
			assertSourceMissingState(t, archived)
			assert.Equal(t, 1, archived.MessageCount)
			survivor, err := archive.GetSession(
				t.Context(), "omnigent:0:conv_0000",
			)
			require.NoError(t, err)
			assert.NotNil(t, survivor)
		})
	}
	assert.Equal(t, observed[130], observed[1030],
		"scheduled work must not grow with the conversation archive")
}

func TestSyncPathsOmnigentSchemaChangeHonorsLegacyDeletionState(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tests := []struct {
		name      string
		deleteOld func(*testing.T, *db.DB, string)
		assertOld func(*testing.T, *db.DB, string)
	}{
		{
			name: "trashed",
			deleteOld: func(t *testing.T, archive *db.DB, id string) {
				t.Helper()
				require.NoError(t, archive.SoftDeleteSession(id))
			},
			assertOld: func(t *testing.T, archive *db.DB, id string) {
				t.Helper()
				assert.True(t, archive.IsSessionTrashed(id),
					"legacy user-trash state must remain recoverable")
			},
		},
		{
			name: "permanently_excluded",
			deleteOld: func(t *testing.T, archive *db.DB, id string) {
				t.Helper()
				require.NoError(t, archive.DeleteSession(id))
			},
			assertOld: func(t *testing.T, archive *db.DB, id string) {
				t.Helper()
				assert.True(t, archive.IsSessionExcluded(id),
					"legacy permanent exclusion must remain recorded")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dbPath := writeOmnigentSplitSyncDB(t, root, 1)
			archive := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(archive, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentOmnigent: {root},
				},
				Machine: "local",
			})
			t.Cleanup(engine.Close)
			engine.SyncPaths([]string{dbPath})
			legacyID := "omnigent:0:conv_0000"
			tc.deleteOld(t, archive, legacyID)

			migrateOmnigentSplitSyncDBWorkspace(t, dbPath, 7, "conv_0000")
			engine.SyncPaths([]string{dbPath})

			tc.assertOld(t, archive, legacyID)
		})
	}
}

func TestSyncOmnigentRetiresDeletedConversationAndPreservesSurvivors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 65)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	syncOmnigentArchive(t, engine, archive, 65)
	deleted, err := archive.GetSession(context.Background(), "omnigent:0:conv_0064")
	require.NoError(t, err)
	require.NotNil(t, deleted)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`DELETE FROM conversation_items
		  WHERE workspace_id = 0 AND conversation_id = 'conv_0064'`)
	require.NoError(t, err)
	_, err = writer.Exec(
		`DELETE FROM omnigent_conversation_metadata
		  WHERE workspace_id = 0 AND id = 'conv_0064'`)
	require.NoError(t, err)
	_, err = writer.Exec(
		`DELETE FROM conversations WHERE workspace_id = 0 AND id = 'conv_0064'`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine.SyncAll(context.Background(), nil)
	deleted, err = archive.GetSession(context.Background(), "omnigent:0:conv_0064")
	require.NoError(t, err)
	assert.NotNil(t, deleted)
	archived, err := archive.GetSessionFull(
		context.Background(), "omnigent:0:conv_0064",
	)
	require.NoError(t, err)
	assertSourceMissingState(t, archived)
	assert.Equal(t, 1, archived.MessageCount,
		"source-missing retirement must preserve archived messages")
	survivor, err := archive.GetSession(
		context.Background(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	assert.NotNil(t, survivor)
}

// TestSyncOmnigentCwdFilterDeletionAppliesWithUnchangedSurvivors pins the
// per-member cwd gate for vanished members: deleting an allowed member
// while every survivor is unchanged leaves the container pass with zero
// retained results, so a source-wide "any allowed result" gate would
// freeze the deletion forever once the container fingerprint is cached.
// The archived cwd of the missing member itself must decide.
func TestSyncOmnigentCwdFilterDeletionAppliesWithUnchangedSurvivors(
	t *testing.T,
) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 2)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine:            "local",
		IncludeCwdPrefixes: []string{"/work"},
	})
	defer engine.Close()
	syncOmnigentArchive(t, engine, archive, 2)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	for _, stmt := range []string{
		`DELETE FROM conversation_items
		  WHERE workspace_id = 0 AND conversation_id = 'conv_0000'`,
		`DELETE FROM omnigent_conversation_metadata
		  WHERE workspace_id = 0 AND id = 'conv_0000'`,
		`DELETE FROM conversations WHERE workspace_id = 0 AND id = 'conv_0000'`,
	} {
		_, err = writer.Exec(stmt)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	engine.SyncAll(context.Background(), nil)
	retired, err := archive.GetSessionFull(
		context.Background(), "omnigent:0:conv_0000",
	)
	require.NoError(t, err)
	assertSourceMissingState(t, retired)
	survivor, err := archive.GetSession(
		context.Background(), "omnigent:0:conv_0001",
	)
	require.NoError(t, err)
	assert.NotNil(t, survivor)
}

// TestSyncOmnigentCwdFilterFreezesDisallowedMissingMember pins the other
// half of the per-member gate: in a mixed-cwd container, a vanished
// member whose archived cwd is outside the allow-list stays frozen (its
// archived row remains active) even though an allowed member's deletion
// in the same pass is applied — a source-wide gate would retire both.
func TestSyncOmnigentCwdFilterFreezesDisallowedMissingMember(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 3)
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`UPDATE omnigent_conversation_metadata
		SET workspace = '/other/place' WHERE workspace_id = 0 AND id = 'conv_0001'`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	archive := dbtest.OpenTestDB(t)
	unfiltered := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	syncOmnigentArchive(t, unfiltered, archive, 3)
	unfiltered.Close()

	writer, err = sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	for _, id := range []string{"conv_0000", "conv_0001"} {
		for _, stmt := range []string{
			`DELETE FROM conversation_items
			  WHERE workspace_id = 0 AND conversation_id = ?`,
			`DELETE FROM omnigent_conversation_metadata
			  WHERE workspace_id = 0 AND id = ?`,
			`DELETE FROM conversations WHERE workspace_id = 0 AND id = ?`,
		} {
			_, err = writer.Exec(stmt, id)
			require.NoError(t, err)
		}
	}
	require.NoError(t, writer.Close())

	filtered := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine:            "local",
		IncludeCwdPrefixes: []string{"/work"},
	})
	defer filtered.Close()
	filtered.SyncAll(context.Background(), nil)

	retired, err := archive.GetSessionFull(
		context.Background(), "omnigent:0:conv_0000",
	)
	require.NoError(t, err)
	assertSourceMissingState(t, retired)

	frozen, err := archive.GetSession(
		context.Background(), "omnigent:0:conv_0001",
	)
	require.NoError(t, err)
	assert.NotNil(t, frozen,
		"a missing member outside the allow-list must stay frozen, not be retired")
}

func TestReconcileOmnigentMissingContainerPreservesArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 2)
	archive := dbtest.OpenTestDB(t)
	var parseCount atomic.Int64
	factory := omnigentParseCountingFactory{
		delegate: omnigentDefaultProviderFactory(t),
		count:    &parseCount,
	}
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine:           "local",
		ProviderFactories: []parser.ProviderFactory{factory},
	})
	syncOmnigentArchive(t, engine, archive, 2)
	require.NoError(t, os.Remove(dbPath))

	parseCount.Store(0)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
	assert.Equal(t, int64(1), parseCount.Load(),
		"the missing container event must reach the persistent provider")
	for _, id := range []string{"omnigent:0:conv_0000", "omnigent:0:conv_0001"} {
		session, err := archive.GetSession(t.Context(), id)
		require.NoError(t, err)
		assert.NotNil(t, session,
			"a vanished persistent container cannot prove member deletion")
	}
}

func TestSyncOmnigentUnsupportedSchemaPreservesArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 1)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	engine.SyncAll(context.Background(), nil)
	before, err := archive.GetSession(context.Background(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, before)

	migrateOmnigentSyncDBToLegacyShape(t, dbPath)

	firstUnsupported := engine.SyncAll(context.Background(), nil)
	assert.Zero(t, firstUnsupported.Failed)
	engine.Close()
	restarted := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	t.Cleanup(restarted.Close)
	secondUnsupported := restarted.SyncAll(context.Background(), nil)
	assert.Zero(t, secondUnsupported.Failed,
		"a cached unsupported source must remain a clean skip")
	after, err := archive.GetSession(context.Background(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, after, "unsupported source must not retire archived sessions")
	assert.Equal(t, before.MessageCount, after.MessageCount)
}

func TestReconcileOmnigentUnsupportedSchemaIsNonfatalAndPreservesArchive(
	t *testing.T,
) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSplitSyncDB(t, root, 1)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	syncOmnigentArchive(t, engine, archive, 1)

	migrateOmnigentSyncDBToLegacyShape(t, dbPath)

	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{root}, false,
	))
	archived, err := archive.GetSessionFull(
		t.Context(), "omnigent:0:conv_0000",
	)
	require.NoError(t, err)
	require.NotNil(t, archived)
	assert.Nil(t, archived.DeletedAt)
	assert.Equal(t, 1, archived.MessageCount)
}

// omnigentBinaryIDSyncDDL mirrors the current pinned Omnigent generation used
// by internal/parser (omnigentBinaryIDGenDDL): id columns are 16-byte UUID
// BLOBs rather than the text ids omnigentSplitSyncDDL above exercises.
const omnigentBinaryIDSyncDDL = `
CREATE TABLE conversations (
	id BLOB NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	title VARCHAR(768) DEFAULT ('') NOT NULL,
	parent_conversation_id BLOB, root_conversation_id BLOB NOT NULL,
	next_position INTEGER, workspace_id BIGINT DEFAULT '0' NOT NULL,
	agent_id BLOB, session_overrides VARCHAR(512),
	archived BOOLEAN DEFAULT 0 NOT NULL,
	PRIMARY KEY (workspace_id, id)
);
CREATE INDEX ix_conversations_archived_updated
	ON conversations(workspace_id, archived, updated_at, id);
CREATE TABLE omnigent_conversation_metadata (
	workspace_id BIGINT DEFAULT '0' NOT NULL, id BLOB NOT NULL,
	kind SMALLINT NOT NULL, sub_agent_name VARCHAR(128),
	external_session_id VARCHAR(128), session_usage BLOB,
	workspace VARCHAR(2048), git_branch VARCHAR(255),
	PRIMARY KEY (workspace_id, id)
);
CREATE TABLE conversation_items (
	id BLOB NOT NULL, conversation_id BLOB NOT NULL,
	response_id VARCHAR(64) NOT NULL, created_at INTEGER NOT NULL,
	position INTEGER NOT NULL, type SMALLINT NOT NULL,
	status SMALLINT NOT NULL, data TEXT NOT NULL, search_text TEXT NOT NULL,
	workspace_id BIGINT DEFAULT '0' NOT NULL,
	PRIMARY KEY (workspace_id, conversation_id, id, created_at)
);
CREATE INDEX ix_conversation_items_conversation_id_position
	ON conversation_items(workspace_id, conversation_id, position);`

// writeOmnigentBinaryIDSyncDB builds a single-conversation chat.db under the
// current binary-uuid generation and returns its path plus the lowercase hex
// form of the conversation id, the form the archived session ID is keyed on.
func writeOmnigentBinaryIDSyncDB(t *testing.T, root string) (string, string) {
	t.Helper()
	path := filepath.Join(root, "chat.db")
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = database.Exec(
		`CREATE TABLE alembic_version (version_num VARCHAR(32) NOT NULL)`,
	)
	require.NoError(t, err)
	for _, statement := range splitSQLStatements(omnigentBinaryIDSyncDDL) {
		_, err = database.Exec(statement)
		require.NoError(t, err)
	}
	_, err = database.Exec(
		`INSERT INTO alembic_version VALUES ('binary-id-sync-test')`,
	)
	require.NoError(t, err)

	convID, err := hex.DecodeString("11112222333344445555666677778888")
	require.NoError(t, err)
	_, err = database.Exec(`INSERT INTO conversations
		(id, created_at, updated_at, title, root_conversation_id, workspace_id)
		VALUES (?, 1700000000, 1700000001, 'binary uuid session', ?, 0)`,
		convID, convID)
	require.NoError(t, err)
	_, err = database.Exec(`INSERT INTO omnigent_conversation_metadata
		(workspace_id, id, kind, workspace)
		VALUES (0, ?, 1, '/work/project')`, convID)
	require.NoError(t, err)
	itemID, err := hex.DecodeString("00000000000000000000000000000001")
	require.NoError(t, err)
	_, err = database.Exec(`INSERT INTO conversation_items
		(id, conversation_id, response_id, created_at, position, type, status,
		 data, search_text, workspace_id)
		VALUES (?, ?, 'resp', 1700000000, 0, 1, 1, ?, 'hi', 0)`,
		itemID, convID,
		`{"role":"user","content":[{"type":"input_text","text":"hi"}]}`)
	require.NoError(t, err)
	require.NoError(t, database.Close())
	return path, hex.EncodeToString(convID)
}

// TestSyncOmnigentBinaryIDGenerationLandsUnderHexSessionID covers the
// current-generation (binary-uuid) chat.db end to end through the sync
// engine: every other Omnigent sync test seeds the older split-schema,
// text-id generation via writeOmnigentSplitSyncDB, leaving the binary-id
// generation's sync path (as opposed to parser-level parsing, already
// covered by TestOmnigentBinaryIDGenerationParses) unexercised.
func TestSyncOmnigentBinaryIDGenerationLandsUnderHexSessionID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	_, convHex := writeOmnigentBinaryIDSyncDB(t, root)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	syncOmnigentArchive(t, engine, archive, 1)

	sessionID := "omnigent:0:" + convHex
	session, err := archive.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, session,
		"a binary-uuid conversation must sync under its hex session ID")
	assert.Equal(t, 1, session.MessageCount)
}
