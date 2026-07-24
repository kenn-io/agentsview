package sync_test

import (
	"context"
	"database/sql"
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

type omnigentForceFullFactory struct {
	delegate parser.ProviderFactory
}

func (f omnigentForceFullFactory) Definition() parser.AgentDef {
	return f.delegate.Definition()
}

func (f omnigentForceFullFactory) Capabilities() parser.Capabilities {
	return f.delegate.Capabilities()
}

func (f omnigentForceFullFactory) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	cfg.ForceFullDiscovery = true
	return f.delegate.NewProvider(cfg)
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
	restorer, ok := p.Provider.(parser.CachedSourceStateRestorer)
	if !ok {
		return false, nil
	}
	return restorer.RestoreCachedSourceState(ctx, source)
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

const omnigentSyncDDL = `
CREATE TABLE alembic_version (version_num VARCHAR(32) NOT NULL);
CREATE TABLE conversations (
	id VARCHAR(64) PRIMARY KEY,
	created_at INTEGER, updated_at INTEGER, title TEXT,
	kind VARCHAR(32), model_override VARCHAR(128),
	parent_conversation_id VARCHAR(64), root_conversation_id VARCHAR(64),
	sub_agent_name VARCHAR(128), workspace VARCHAR(2048),
	git_branch VARCHAR(255), session_usage TEXT
);
CREATE INDEX ix_conversations_updated_at ON conversations(updated_at, id);
CREATE TABLE conversation_items (
	id VARCHAR(64) PRIMARY KEY, conversation_id VARCHAR(64) NOT NULL,
	position INTEGER NOT NULL, type VARCHAR(32) NOT NULL,
	data TEXT NOT NULL, search_text TEXT NOT NULL
);
CREATE INDEX ix_conversation_items_conversation_id_position
	ON conversation_items(conversation_id, position);`

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
CREATE TABLE conversation_items (
	workspace_id BIGINT NOT NULL DEFAULT 0,
	conversation_id VARCHAR(64) NOT NULL, id VARCHAR(64) NOT NULL,
	position INTEGER NOT NULL, type SMALLINT NOT NULL,
	data TEXT NOT NULL, search_text TEXT NOT NULL,
	PRIMARY KEY (workspace_id, conversation_id, id)
);
CREATE INDEX ix_conversation_items_conversation_id_position
	ON conversation_items(workspace_id, conversation_id, position);`

func writeOmnigentSyncDB(t *testing.T, root string, count int) string {
	t.Helper()
	path := filepath.Join(root, "chat.db")
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	for _, statement := range splitSQLStatements(omnigentSyncDDL) {
		_, err = database.Exec(statement)
		require.NoError(t, err)
	}
	_, err = database.Exec(`INSERT INTO alembic_version VALUES ('sync-test')`)
	require.NoError(t, err)
	for i := range count {
		id := fmt.Sprintf("conv_%04d", i)
		updatedAt := int64(1_700_000_000 + i)
		_, err = database.Exec(`INSERT INTO conversations
			(id, created_at, updated_at, title, kind, root_conversation_id)
			VALUES (?, ?, ?, ?, 'default', ?)`,
			id, updatedAt-1, updatedAt, id, id)
		require.NoError(t, err)
		_, err = database.Exec(`INSERT INTO conversation_items
			(id, conversation_id, position, type, data, search_text)
			VALUES (?, ?, 0, 'message', ?, 'initial')`, id+"_0", id,
			`{"role":"user","content":[{"type":"input_text","text":"initial"}]}`)
		require.NoError(t, err)
	}
	require.NoError(t, database.Close())
	return path
}

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
	for i := range count {
		id := fmt.Sprintf("conv_%04d", i)
		updatedAt := int64(1_700_000_000 + i)
		_, err = database.Exec(`INSERT INTO conversations
			(workspace_id, id, created_at, updated_at, title, root_conversation_id)
			VALUES (0, ?, ?, ?, ?, ?)`,
			id, updatedAt-1, updatedAt, id, id)
		require.NoError(t, err)
		_, err = database.Exec(`INSERT INTO omnigent_conversation_metadata
			(workspace_id, id, kind, workspace)
			VALUES (0, ?, 1, '/work/project')`, id)
		require.NoError(t, err)
		_, err = database.Exec(`INSERT INTO conversation_items
			(workspace_id, conversation_id, id, position, type, data, search_text)
			VALUES (0, ?, ?, 0, 1, ?, 'initial')`, id, id+"_0",
			`{"role":"user","content":[{"type":"input_text","text":"initial"}]}`)
		require.NoError(t, err)
	}
	require.NoError(t, database.Close())
	return path
}

func migrateOmnigentSyncDBToSplit(
	t *testing.T, path string, workspaceID int64, conversationIDs ...string,
) {
	t.Helper()
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	_, err = database.Exec(`DROP TABLE conversation_items`)
	require.NoError(t, err)
	_, err = database.Exec(`DROP TABLE conversations`)
	require.NoError(t, err)
	for _, statement := range splitSQLStatements(omnigentSplitSyncDDL) {
		_, err = database.Exec(statement)
		require.NoError(t, err)
	}
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
	for _, archiveSize := range []int{5, 200} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			root := t.TempDir()
			dbPath := writeOmnigentSyncDB(t, root, archiveSize)
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
				`UPDATE conversations SET updated_at = ? WHERE id = ?`,
				changedAt, changedID)
			require.NoError(t, err)
			_, err = writer.Exec(`INSERT INTO conversation_items
				(id, conversation_id, position, type, data, search_text)
				VALUES (?, ?, 1, 'message', ?, 'changed')`, changedID+"_1", changedID,
				`{"role":"assistant","content":[{"type":"output_text","text":"changed"}]}`)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			engine.SyncPaths([]string{dbPath + "-wal"})
			assert.Equal(t, 1, engine.LastSyncStats().Synced,
				"one changed conversation should produce one archive write")
			changed, err := archive.GetSessionFull(
				context.Background(), "omnigent:"+changedID)
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
				context.Background(), "omnigent:"+unchangedID)
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
				`DELETE FROM conversation_items WHERE conversation_id = 'conv_0001'`)
			require.NoError(t, err)
			_, err = tx.Exec(`DELETE FROM conversations WHERE id = 'conv_0001'`)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO conversations
				(id, created_at, updated_at, title, kind, root_conversation_id)
				VALUES ('replacement', 1, ?, 'replacement',
					'default', 'replacement')`, time.Now().Unix())
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO conversation_items
				(id, conversation_id, position, type, data, search_text)
				VALUES ('replacement_0', 'replacement', 0, 'message', ?, 'replacement')`,
				`{"role":"user","content":[{"type":"input_text","text":"replacement"}]}`)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			require.NoError(t, writer.Close())

			engine.SyncPaths([]string{dbPath})
			deleted, err := archive.GetSession(
				context.Background(), "omnigent:conv_0001")
			require.NoError(t, err)
			assert.NotNil(t, deleted,
				"changed-path work must defer archive-wide deletion proof")
			replacement, err := archive.GetSession(
				context.Background(), "omnigent:replacement")
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
	writeOmnigentSyncDB(t, root, 200)
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
	dbPath := writeOmnigentSyncDB(t, root, 3)
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
	session, err := archive.GetSession(context.Background(), "omnigent:conv_0000")
	require.NoError(t, err)
	assert.NotNil(t, session, "the failed physical source must remain retryable")
}

func TestSyncPathsOmnigentFailedMemberRetryReplaysContainer(t *testing.T) {
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

	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
	assert.Zero(t, engine.LastSyncStats().Failed)
	updated, err := archive.GetSessionFull(t.Context(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, 2, updated.MessageCount,
		"retrying the same watcher path must replay the stale member")
}

func TestSyncAllSinceOmnigentFailedMemberRetrySurvivesFutureCutoff(t *testing.T) {
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
	updated, err := archive.GetSessionFull(t.Context(), "omnigent:0:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, 2, updated.MessageCount,
		"a pending full retry must bypass a cutoff newer than the container")

	stats = engine.SyncAll(t.Context(), nil)
	assert.Zero(t, stats.Synced,
		"a successful forced retry must acknowledge its generation")
}

func TestOmnigentFailedRetrySurvivesUnrelatedScopedSync(t *testing.T) {
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
				"unrelated scoped work must not acknowledge root A's retry")
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
	for _, archiveSize := range []int{200, 2000} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			root := t.TempDir()
			dbPath := writeOmnigentSyncDB(t, root, archiveSize)
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
				SET updated_at = ? WHERE id = 'conv_0000'`, time.Now().Unix())
			require.NoError(t, err)
			_, err = writer.Exec(`INSERT INTO conversation_items
				(id, conversation_id, position, type, data, search_text)
				VALUES ('changed', 'conv_0000', 1, 'message', ?, 'changed')`,
				`{"role":"assistant","content":[{"type":"output_text","text":"changed"}]}`)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			parseCount.Store(0)
			resultCount.Store(0)
			engine.SyncAll(context.Background(), nil)
			assert.Equal(t, 1, engine.LastSyncStats().Synced,
				"only the changed member may be rewritten")
			assert.Equal(t, int64(1), parseCount.Load(),
				"a changed container parses only the changed member")
			assert.Equal(t, int64(1), resultCount.Load(),
				"periodic work must stay constant as the archive grows")
		})
	}
}

func TestSyncOmnigentRestartCacheWarmsBoundedChangeTracker(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	var watcherResultCounts []int64
	for _, archiveSize := range []int{200, 2000} {
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
			assert.LessOrEqual(t, resultCount.Load(), int64(129),
				"restart replay must stay within the changed member plus "+
					"the fixed recent-member window")
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
	writeOmnigentSyncDB(t, root, 3)
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
	dbPath := writeOmnigentSyncDB(t, root, 2)
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
		`DELETE FROM conversation_items WHERE conversation_id = 'conv_0001'`,
	)
	require.NoError(t, err)
	_, err = writer.Exec(`DELETE FROM conversations WHERE id = 'conv_0001'`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	forceEngine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
		ProviderFactories: []parser.ProviderFactory{omnigentForceFullFactory{
			delegate: omnigentDefaultProviderFactory(t),
		}},
	})
	t.Cleanup(forceEngine.Close)
	stats := forceEngine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	active, err := archive.GetSession(t.Context(), "omnigent:conv_0001")
	require.NoError(t, err)
	assert.Nil(t, active)
	archived, err := archive.GetSessionFull(t.Context(), "omnigent:conv_0001")
	require.NoError(t, err)
	require.NotNil(t, archived,
		"complete container parsing must retain the source-missing archive row")
	require.NotNil(t, archived.DeletionCause)
	assert.Equal(t, "source_missing", *archived.DeletionCause)
	assert.Equal(t, 1, archived.MessageCount)
}

func TestSyncOmnigentCompleteEmptyContainerPreservesFinalConversation(
	t *testing.T,
) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSyncDB(t, root, 1)
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
	_, err = writer.Exec(`DELETE FROM conversations`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	forceEngine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
		ProviderFactories: []parser.ProviderFactory{omnigentForceFullFactory{
			delegate: omnigentDefaultProviderFactory(t),
		}},
	})
	t.Cleanup(forceEngine.Close)
	stats := forceEngine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	active, err := archive.GetSession(t.Context(), "omnigent:conv_0000")
	require.NoError(t, err)
	assert.Nil(t, active)
	archived, err := archive.GetSessionFull(t.Context(), "omnigent:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, archived,
		"deleting the final source conversation must retain its archive row")
	require.NotNil(t, archived.DeletionCause)
	assert.Equal(t, "source_missing", *archived.DeletionCause)
	assert.Equal(t, 1, archived.MessageCount)
}

func TestSyncOmnigentDataVersionFailurePreventsContainerCache(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	claudeRoot := t.TempDir()
	writeOmnigentSyncDB(t, root, 2)
	archive := dbtest.OpenTestDB(t)
	raw, err := sql.Open("sqlite3", archive.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	_, err = raw.Exec(`CREATE TRIGGER fail_omnigent_data_version
		BEFORE UPDATE OF data_version ON sessions
		WHEN NEW.id = 'omnigent:conv_0000'
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
	assert.Less(t, archive.GetSessionDataVersion("omnigent:conv_0000"),
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
		archive.GetSessionDataVersion("omnigent:conv_0000"))
}

func TestSyncOmnigentFailedCurrentUpdateForcesContentReplacement(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSyncDB(t, root, 1)
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
		archive.GetSessionDataVersion("omnigent:conv_0000"))

	raw, err := sql.Open("sqlite3", archive.Path())
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.Exec(`CREATE TRIGGER fail_omnigent_message_append
		BEFORE INSERT ON messages
		WHEN NEW.session_id = 'omnigent:conv_0000' AND NEW.ordinal = 1
		BEGIN
			SELECT RAISE(FAIL, 'injected message append failure');
		END`)
	require.NoError(t, err)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`UPDATE conversations SET updated_at = ?
		WHERE id = 'conv_0000'`, time.Now().Unix())
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO conversation_items
		(id, conversation_id, position, type, data, search_text)
		VALUES ('conv_0000_1', 'conv_0000', 1, 'message', ?, 'second')`,
		`{"role":"assistant","content":[{"type":"output_text","text":"second"}]}`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 1, engine.LastSyncStats().Failed)
	assert.Less(t, archive.GetSessionDataVersion("omnigent:conv_0000"),
		db.CurrentDataVersion(),
		"an incomplete current-session update must persist retry state")

	_, err = raw.Exec(`DROP TRIGGER fail_omnigent_message_append`)
	require.NoError(t, err)
	engine.SyncAll(context.Background(), nil)
	messages, err := archive.GetMessages(
		context.Background(), "omnigent:conv_0000", 0, 10, true,
	)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "second", messages[1].Content)
}

func TestSyncOmnigentPersistsJSONStringToolResult(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSyncDB(t, root, 1)
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO conversation_items
		(id, conversation_id, position, type, data, search_text) VALUES
		('call', 'conv_0000', 1, 'function_call',
		 '{"call_id":"call-json","name":"inspect","arguments":"{}"}', ''),
		('result', 'conv_0000', 2, 'function_call_output',
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

	messages := fetchMessages(t, archive, "omnigent:conv_0000")
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
	dbPath := writeOmnigentSyncDB(t, root, 1)
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`UPDATE conversations
		SET model_override = 'claude-sonnet',
		    session_usage = '{"input_tokens":120,"output_tokens":30,"total_cost_usd":0.25}'
		WHERE id = 'conv_0000'`)
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

	events, err := archive.GetUsageEvents(context.Background(), "omnigent:conv_0000")
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
	assert.InDelta(t, 0.25, daily.Daily[0].TotalCost, 0.0001)
}

func TestSyncOmnigentSameTimestampAppendIsReconciledByFullSync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSyncDB(t, root, 1)
	beforeInfo, err := os.Stat(dbPath)
	require.NoError(t, err)
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
	_, err = writer.Exec(`INSERT INTO conversation_items
		(id, conversation_id, position, type, data, search_text)
		VALUES ('conv_0000_1', 'conv_0000', 1, 'message', ?, 'appended')`,
		`{"role":"assistant","content":[{"type":"output_text","text":"appended"}]}`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	afterInfo, err := os.Stat(dbPath)
	require.NoError(t, err)
	require.Equal(t, beforeInfo.Size(), afterInfo.Size(),
		"fixture must preserve the container size to exercise hash freshness")

	// An append that advances neither updated_at nor the container size is
	// invisible to the changed-member sweep: the event stays bounded by the
	// changed set and defers the edit instead of probing every member.
	engine.SyncPaths([]string{dbPath})
	deferred, err := archive.GetSessionFull(context.Background(), "omnigent:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, deferred)
	assert.Equal(t, 1, deferred.MessageCount,
		"the changed-path sweep must defer an edit it cannot see")

	engine = sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	engine.SyncAll(context.Background(), nil)
	updated, err := archive.GetSessionFull(context.Background(), "omnigent:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, 2, updated.MessageCount,
		"the scheduled full sync must reconcile edits the sweep deferred")
}

func TestSyncOmnigentArchiveAuditDetectsInPlaceItemEdit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSyncDB(t, root, 1)
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
		WHERE id = 'conv_0000_0'`,
		`{"role":"user","content":[{"type":"input_text","text":"edited"}]}`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	require.NoError(t, engine.ReconcileWatchRoots(
		t.Context(), []string{root}, false,
	))
	messages, err := archive.GetAllMessages(
		context.Background(), "omnigent:conv_0000")
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

func TestScheduledOmnigentReconciliationIsBoundedByChangedMembers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	observed := make(map[int]int64)
	for _, archiveSize := range []int{256, 1024} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			root := t.TempDir()
			dbPath := writeOmnigentSyncDB(t, root, archiveSize)
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
			_, err = writer.Exec(
				`UPDATE conversations SET updated_at = ? WHERE id = ?`,
				time.Now().Unix(), fmt.Sprintf("conv_%04d", archiveSize/2),
			)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			parseCount.Store(0)
			require.NoError(t, engine.ReconcileProviderRoots(
				t.Context(), parser.AgentOmnigent, []string{root},
			))
			observed[archiveSize] = parseCount.Load()
			assert.Equal(t, int64(1), parseCount.Load(),
				"scheduled reconciliation should parse only the changed member")

			deletedID := fmt.Sprintf("conv_%04d", archiveSize-1)
			writer, err = sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			_, err = writer.Exec(
				`DELETE FROM conversation_items WHERE conversation_id = ?`,
				deletedID,
			)
			require.NoError(t, err)
			_, err = writer.Exec(
				`DELETE FROM conversations WHERE id = ?`, deletedID,
			)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			require.NoError(t, engine.ReconcileProviderRoots(
				t.Context(), parser.AgentOmnigent, []string{root},
			))
			session, err := archive.GetSession(
				t.Context(), "omnigent:"+deletedID,
			)
			require.NoError(t, err)
			assert.NotNil(t, session,
				"bounded scheduled discovery cannot prove member deletion")
		})
	}
	assert.Equal(t, observed[256], observed[1024],
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
			dbPath := writeOmnigentSyncDB(t, root, 1)
			archive := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(archive, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentOmnigent: {root},
				},
				Machine: "local",
			})
			t.Cleanup(engine.Close)
			engine.SyncPaths([]string{dbPath})
			legacyID := "omnigent:conv_0000"
			tc.deleteOld(t, archive, legacyID)

			migrateOmnigentSyncDBToSplit(t, dbPath, 7, "conv_0000")
			engine.SyncPaths([]string{dbPath})

			qualified, err := archive.GetSession(
				t.Context(), "omnigent:7:conv_0000",
			)
			require.NoError(t, err)
			assert.Nil(t, qualified,
				"identity migration must not bypass legacy deletion state")
			tc.assertOld(t, archive, legacyID)
		})
	}
}

func TestSyncPathsOmnigentSchemaChangeRetiresLegacyArchiveID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSyncDB(t, root, 2)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	engine.SyncPaths([]string{dbPath})
	legacy, err := archive.GetSession(context.Background(), "omnigent:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, legacy)
	orphan, err := archive.GetSession(context.Background(), "omnigent:conv_0001")
	require.NoError(t, err)
	require.NotNil(t, orphan)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`DROP TABLE conversation_items`)
	require.NoError(t, err)
	_, err = writer.Exec(`DROP TABLE conversations`)
	require.NoError(t, err)
	for _, statement := range splitSQLStatements(omnigentSplitSyncDDL) {
		_, err = writer.Exec(statement)
		require.NoError(t, err)
	}
	_, err = writer.Exec(`INSERT INTO conversations
		(workspace_id, id, created_at, updated_at, title, root_conversation_id)
		VALUES (7, 'conv_0000', 1, 2, 'migrated', 'conv_0000')`)
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO omnigent_conversation_metadata
		(workspace_id, id, kind, workspace)
		VALUES (7, 'conv_0000', 1, '/work/project')`)
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO conversation_items
		(workspace_id, conversation_id, id, position, type, data, search_text)
		VALUES (7, 'conv_0000', 'item', 0, 1, ?, 'migrated')`,
		`{"role":"user","content":[{"type":"input_text","text":"migrated"}]}`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine.SyncPaths([]string{dbPath})
	legacy, err = archive.GetSession(context.Background(), "omnigent:conv_0000")
	require.NoError(t, err)
	assert.Nil(t, legacy)
	orphan, err = archive.GetSession(context.Background(), "omnigent:conv_0001")
	require.NoError(t, err)
	assert.Nil(t, orphan,
		"a legacy member absent from the new schema must be retired")
	qualified, err := archive.GetSession(context.Background(), "omnigent:7:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, qualified)
	assert.Equal(t, "migrated", *qualified.DisplayName)
}

func TestReconcileOmnigentRetiresDeletedConversationAndPreservesSurvivors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSyncDB(t, root, 65)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	syncOmnigentArchive(t, engine, archive, 65)
	deleted, err := archive.GetSession(context.Background(), "omnigent:conv_0064")
	require.NoError(t, err)
	require.NotNil(t, deleted)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(
		`DELETE FROM conversation_items WHERE conversation_id = 'conv_0064'`)
	require.NoError(t, err)
	_, err = writer.Exec(`DELETE FROM conversations WHERE id = 'conv_0064'`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine.SyncAll(context.Background(), nil)
	deleted, err = archive.GetSession(context.Background(), "omnigent:conv_0064")
	require.NoError(t, err)
	require.NotNil(t, deleted,
		"routine discovery defers deletion proof to authoritative reconciliation")

	require.NoError(t, engine.ReconcileWatchRoots(
		t.Context(), []string{root}, false,
	))
	deleted, err = archive.GetSession(context.Background(), "omnigent:conv_0064")
	require.NoError(t, err)
	assert.Nil(t, deleted)
	archived, err := archive.GetSessionFull(
		context.Background(), "omnigent:conv_0064",
	)
	require.NoError(t, err)
	require.NotNil(t, archived,
		"authoritative reconciliation must preserve the archived session row")
	require.NotNil(t, archived.DeletionCause)
	assert.Equal(t, "source_missing", *archived.DeletionCause)
	assert.Equal(t, 1, archived.MessageCount,
		"source-missing reconciliation must preserve archived messages")
	survivor, err := archive.GetSession(context.Background(), "omnigent:conv_0000")
	require.NoError(t, err)
	assert.NotNil(t, survivor)
}

func TestReconcileOmnigentMissingContainerPreservesArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	dbPath := writeOmnigentSyncDB(t, root, 2)
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
	for _, id := range []string{"omnigent:conv_0000", "omnigent:conv_0001"} {
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
	dbPath := writeOmnigentSyncDB(t, root, 1)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	engine.SyncAll(context.Background(), nil)
	before, err := archive.GetSession(context.Background(), "omnigent:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, before)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`DROP TABLE conversation_items`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	engine.SyncAll(context.Background(), nil)
	after, err := archive.GetSession(context.Background(), "omnigent:conv_0000")
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
	dbPath := writeOmnigentSyncDB(t, root, 1)
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
	_, err = writer.Exec(`DROP TABLE conversation_items`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{root}, false,
	))
	archived, err := archive.GetSessionFull(
		t.Context(), "omnigent:conv_0000",
	)
	require.NoError(t, err)
	require.NotNil(t, archived)
	assert.Nil(t, archived.DeletedAt)
	assert.Equal(t, 1, archived.MessageCount)
}
