// ABOUTME: Regression tests for Omnigent state across full archive rebuilds.
// ABOUTME: Ensures discarded resync work is retried against the live archive.
package sync

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

type cancelAfterOmnigentParseFactory struct {
	delegate parser.ProviderFactory
	cancel   context.CancelFunc
	once     *stdsync.Once
}

func (f cancelAfterOmnigentParseFactory) Definition() parser.AgentDef {
	return f.delegate.Definition()
}

func (f cancelAfterOmnigentParseFactory) Capabilities() parser.Capabilities {
	return f.delegate.Capabilities()
}

func (f cancelAfterOmnigentParseFactory) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	return &cancelAfterOmnigentParseProvider{
		Provider: f.delegate.NewProvider(cfg), cancel: f.cancel, once: f.once,
	}
}

type cancelAfterOmnigentParseProvider struct {
	parser.Provider
	cancel context.CancelFunc
	once   *stdsync.Once
}

func (p *cancelAfterOmnigentParseProvider) Parse(
	ctx context.Context, req parser.ParseRequest,
) (parser.ParseOutcome, error) {
	outcome, err := p.Provider.Parse(ctx, req)
	if err == nil {
		p.once.Do(p.cancel)
	}
	return outcome, err
}

func writeOmnigentResyncSource(t *testing.T, root string) string {
	t.Helper()
	sourcePath := filepath.Join(root, "chat.db")
	writer, err := sql.Open("sqlite3", sourcePath)
	require.NoError(t, err)
	statements := []string{
		`CREATE TABLE alembic_version (version_num VARCHAR(32) NOT NULL)`,
		`CREATE TABLE conversations (
			id VARCHAR(64) PRIMARY KEY,
			created_at INTEGER, updated_at INTEGER, title TEXT,
			kind VARCHAR(32), model_override VARCHAR(128),
			parent_conversation_id VARCHAR(64), root_conversation_id VARCHAR(64),
			sub_agent_name VARCHAR(128), workspace VARCHAR(2048),
			git_branch VARCHAR(255), session_usage TEXT
		)`,
		`CREATE TABLE conversation_items (
			id VARCHAR(64) PRIMARY KEY, conversation_id VARCHAR(64) NOT NULL,
			position INTEGER NOT NULL, type VARCHAR(32) NOT NULL,
			data TEXT NOT NULL, search_text TEXT NOT NULL
		)`,
		`INSERT INTO alembic_version VALUES ('resync-test')`,
		`INSERT INTO conversations
			(id, created_at, updated_at, title, kind, root_conversation_id)
			VALUES ('conversation', 1699999999, 1700000000,
			        'conversation', 'default', 'conversation')`,
		`INSERT INTO conversation_items
			(id, conversation_id, position, type, data, search_text)
			VALUES ('item-0', 'conversation', 0, 'message',
			        '{"role":"user","content":[{"type":"input_text","text":"initial"}]}',
			        'initial')`,
	}
	for _, statement := range statements {
		_, err = writer.Exec(statement)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return sourcePath
}

func TestAbortedResyncQueuesOmnigentContainer(t *testing.T) {
	root := t.TempDir()
	sourcePath := writeOmnigentResyncSource(t, root)
	writer, err := sql.Open("sqlite3", sourcePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })

	archive := openTestDB(t)
	engine := NewEngine(archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)

	_, err = writer.Exec(
		`UPDATE conversations SET updated_at = 1800000000 WHERE id = 'conversation'`,
	)
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO conversation_items
		(id, conversation_id, position, type, data, search_text)
		VALUES ('item-1', 'conversation', 1, 'message',
		        '{"role":"assistant","content":[{"type":"output_text","text":"changed"}]}',
		        'changed')`)
	require.NoError(t, err)

	sentinel := errors.New("fts sentinel")
	engine.syncMu.Lock()
	stats, err := engine.resyncAllWithOptionsLocked(
		context.Background(), nil, RebuildOptions{}, rebuildOperations{
			rebuildFTS: func(*db.DB) error { return sentinel },
		},
	)
	engine.syncMu.Unlock()
	require.ErrorIs(t, err, sentinel)
	assert.True(t, stats.Aborted)

	unchanged, err := archive.GetMessages(
		context.Background(), "omnigent:conversation", 0, 10, true,
	)
	require.NoError(t, err)
	assert.Len(t, unchanged, 1,
		"an aborted resync must not publish temporary archive data")

	recovered := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, recovered.Synced)
	updated, err := archive.GetMessages(
		context.Background(), "omnigent:conversation", 0, 10, true,
	)
	require.NoError(t, err)
	assert.Len(t, updated, 2,
		"the next regular sync must retry work discarded with the resync")
}

func TestResyncPreparationFailureDoesNotQueueOmnigentContainer(t *testing.T) {
	root := t.TempDir()
	writeOmnigentResyncSource(t, root)
	archive := openTestDB(t)
	engine := NewEngine(archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	tempPath := archive.Path() + resyncTempSuffix
	require.NoError(t, os.Mkdir(tempPath, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tempPath, "keep"), []byte("non-empty"), 0o600,
	))
	engine.syncMu.Lock()
	stats, err := engine.resyncAllWithOptionsLocked(
		context.Background(), nil, RebuildOptions{}, rebuildOperations{},
	)
	engine.syncMu.Unlock()
	require.Error(t, err)
	assert.True(t, stats.Aborted)
	engine.omnigentRetryMu.Lock()
	retryCount := len(engine.omnigentRetrySources)
	engine.omnigentRetryMu.Unlock()
	assert.Zero(t, retryCount,
		"preparation failure cannot have advanced the provider tracker")
}

func TestCanceledResyncBeforeOmnigentParseDoesNotQueueContainer(t *testing.T) {
	root := t.TempDir()
	writeOmnigentResyncSource(t, root)
	archive := openTestDB(t)
	engine := NewEngine(archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	engine.syncMu.Lock()
	stats, err := engine.resyncAllWithOptionsLocked(
		ctx, nil, RebuildOptions{}, rebuildOperations{},
	)
	engine.syncMu.Unlock()
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, stats.Aborted)
	engine.omnigentRetryMu.Lock()
	retryCount := len(engine.omnigentRetrySources)
	engine.omnigentRetryMu.Unlock()
	assert.Zero(t, retryCount,
		"cancellation before container parse must not force a later full parse")
}

func TestCanceledResyncAfterOmnigentParseQueuesContainer(t *testing.T) {
	root := t.TempDir()
	writeOmnigentResyncSource(t, root)
	archive := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	delegate, ok := parser.ProviderFactoryByType(parser.AgentOmnigent)
	require.True(t, ok)
	factory := cancelAfterOmnigentParseFactory{
		delegate: delegate, cancel: cancel, once: &stdsync.Once{},
	}
	engine := NewEngine(archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {root},
		},
		Machine:           "local",
		ProviderFactories: []parser.ProviderFactory{factory},
	})
	t.Cleanup(engine.Close)

	engine.syncMu.Lock()
	stats, err := engine.resyncAllWithOptionsLocked(
		ctx, nil, RebuildOptions{}, rebuildOperations{},
	)
	engine.syncMu.Unlock()
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, stats.Aborted)
	engine.omnigentRetryMu.Lock()
	retryCount := len(engine.omnigentRetrySources)
	engine.omnigentRetryMu.Unlock()
	assert.Equal(t, 1, retryCount,
		"discarding a parsed container must queue it for the live archive")
}
