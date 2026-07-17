package sync_test

import (
	"context"
	"database/sql"
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
)

type omnigentParseCountingFactory struct {
	delegate parser.ProviderFactory
	count    *atomic.Int64
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
	}
}

type omnigentParseCountingProvider struct {
	parser.Provider
	count *atomic.Int64
}

func (p *omnigentParseCountingProvider) Parse(
	ctx context.Context, req parser.ParseRequest,
) (parser.ParseOutcome, error) {
	p.count.Add(1)
	return p.Provider.Parse(ctx, req)
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
);`

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
);`

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
			engine.SyncAll(context.Background(), nil)
			require.Equal(t, archiveSize, engine.LastSyncStats().Synced)
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
				VALUES ('replacement', 1, 1800000001, 'replacement',
					'default', 'replacement')`)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO conversation_items
				(id, conversation_id, position, type, data, search_text)
				VALUES ('replacement_0', 'replacement', 0, 'message', ?, 'replacement')`,
				`{"role":"user","content":[{"type":"input_text","text":"replacement"}]}`)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			require.NoError(t, writer.Close())

			engine.SyncPaths([]string{dbPath})
			deleted, err := archive.GetSession(context.Background(), "omnigent:conv_0001")
			require.NoError(t, err)
			assert.Nil(t, deleted, "deleted conversation should retire its member session")
			replacement, err := archive.GetSession(
				context.Background(), "omnigent:replacement")
			require.NoError(t, err)
			require.NotNil(t, replacement)
		})
	}
}

func TestSyncOmnigentUnchangedFullSyncUsesContainerCache(t *testing.T) {
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
	engine.SyncAll(context.Background(), nil)
	require.Equal(t, int64(1), parseCount.Load())

	parseCount.Store(0)
	engine.SyncAll(context.Background(), nil)
	assert.Zero(t, parseCount.Load(),
		"unchanged container must not reparse every conversation")
}

func TestSyncOmnigentDataVersionFailurePreventsContainerCache(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
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
	engine := sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine:           "local",
		ProviderFactories: []parser.ProviderFactory{factory},
	})
	engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 1, engine.LastSyncStats().Failed)
	assert.Less(t, archive.GetSessionDataVersion("omnigent:conv_0000"),
		db.CurrentDataVersion())

	_, err = raw.Exec(`DROP TRIGGER fail_omnigent_data_version`)
	require.NoError(t, err)
	parseCount.Store(0)
	engine.SyncAll(context.Background(), nil)
	assert.Equal(t, int64(1), parseCount.Load(),
		"stale virtual member must bypass the container cache")
	assert.Equal(t, db.CurrentDataVersion(),
		archive.GetSessionDataVersion("omnigent:conv_0000"))
}

func TestSyncPathsOmnigentSameTimestampAppendUsesMemberHash(t *testing.T) {
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

	engine.SyncPaths([]string{dbPath})
	assert.Equal(t, 1, engine.LastSyncStats().Synced)
	updated, err := archive.GetSessionFull(context.Background(), "omnigent:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, 2, updated.MessageCount)
}

func TestSyncOmnigentPeriodicFullSyncDetectsInPlaceItemEdit(t *testing.T) {
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

	engine = sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	engine.SyncAll(context.Background(), nil)
	messages, err := archive.GetAllMessages(
		context.Background(), "omnigent:conv_0000")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "edited", messages[0].Content)
}

func TestSyncPathsOmnigentSchemaChangeRetiresLegacyArchiveID(t *testing.T) {
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
	engine.SyncPaths([]string{dbPath})
	legacy, err := archive.GetSession(context.Background(), "omnigent:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, legacy)

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
	qualified, err := archive.GetSession(context.Background(), "omnigent:7:conv_0000")
	require.NoError(t, err)
	require.NotNil(t, qualified)
	assert.Equal(t, "migrated", *qualified.DisplayName)
}

func TestSyncOmnigentPeriodicFullSyncReconcilesDeletedConversation(t *testing.T) {
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
	engine.SyncAll(context.Background(), nil)
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

	engine = sync.NewEngine(archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	engine.SyncAll(context.Background(), nil)
	deleted, err = archive.GetSession(context.Background(), "omnigent:conv_0064")
	require.NoError(t, err)
	assert.Nil(t, deleted)
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
