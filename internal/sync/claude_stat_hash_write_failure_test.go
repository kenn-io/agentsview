package sync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestSyncClaudeForkWriteFailureRetriesWholeSource proves that freshness is a
// source-level outcome for Claude DAG transcripts. One committed branch cannot
// make the shared transcript fresh while another branch still needs a write.
func TestSyncClaudeForkWriteFailureRetriesWholeSource(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	projectDir := filepath.Join(root, "project-a")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "forked.jsonl")
	builder := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:00Z", "start", "a", "",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:01Z", "ok", "b", "a",
		).
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:02Z", "main-2", "c", "b",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:03Z", "ok-2", "d", "c",
		).
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:04Z", "main-3", "e", "d",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:05Z", "ok-3", "f", "e",
		).
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:06Z", "main-4", "g", "f",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:07Z", "ok-4", "h", "g",
		).
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:08Z", "main-5", "k", "h",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:09Z", "ok-5", "l", "k",
		)
	require.NoError(t, os.WriteFile(path, []byte(builder.String()), 0o644))

	initialEngine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "local",
	})
	t.Cleanup(initialEngine.Close)
	initial := initialEngine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, initial.Synced)
	require.Zero(t, initial.Failed)
	require.Equal(t, db.CurrentDataVersion(),
		database.GetSessionDataVersion("forked"))

	builder.AddClaudeUserWithUUID(
		"2024-01-01T10:01:00Z", "fork", "i", "b",
	).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:01:01Z", "fork-ok", "j", "i",
		)
	require.NoError(t, os.WriteFile(path, []byte(builder.String()), 0o644))

	raw, err := sql.Open("sqlite3", database.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	_, err = raw.Exec(`
		CREATE TRIGGER fail_claude_fork_insert
		BEFORE INSERT ON sessions
		WHEN NEW.id = 'forked-i'
		BEGIN
			SELECT RAISE(FAIL, 'injected Claude fork write failure');
		END;
		CREATE TRIGGER fail_claude_main_demotion
		BEFORE UPDATE OF data_version ON sessions
		WHEN NEW.id = 'forked'
		 AND OLD.data_version > NEW.data_version
		BEGIN
			SELECT RAISE(FAIL, 'injected Claude main demotion failure');
		END;
	`)
	require.NoError(t, err)

	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	failedDemotion := engine.SyncAll(context.Background(), nil)
	require.Zero(t, failedDemotion.Synced)
	require.Equal(t, 1, failedDemotion.Failed)
	require.Equal(t, db.CurrentDataVersion(),
		database.GetSessionDataVersion("forked"),
		"a rejected pre-write demotion must abort before changing the row")
	_, hasDigest, err := database.GetProviderStatHash(
		t.Context(), parser.AgentClaude, path,
	)
	require.NoError(t, err)
	assert.False(t, hasDigest,
		"a rejected source demotion must revoke the old digest")

	_, err = raw.Exec(`DROP TRIGGER fail_claude_main_demotion`)
	require.NoError(t, err)
	partialEngine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "local",
	})
	t.Cleanup(partialEngine.Close)
	failed := partialEngine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, failed.Synced)
	require.Equal(t, 1, failed.Failed)
	main, err := database.GetSession(t.Context(), "forked")
	require.NoError(t, err)
	require.NotNil(t, main, "the first DAG branch must commit before the failure")
	assert.Less(t, main.DataVersion, db.CurrentDataVersion(),
		"the committed main branch must remain retryable")
	fork, err := database.GetSession(t.Context(), "forked-i")
	require.NoError(t, err)
	assert.Nil(t, fork, "the injected fork write must leave the branch absent")

	_, hasDigest, err = database.GetProviderStatHash(
		t.Context(), parser.AgentClaude, path,
	)
	require.NoError(t, err)
	assert.False(t, hasDigest,
		"a partial DAG write must not mark the shared source fresh")

	_, err = raw.Exec(`DROP TRIGGER fail_claude_fork_insert`)
	require.NoError(t, err)
	restarted := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "local",
	})
	t.Cleanup(restarted.Close)

	retry := restarted.SyncAll(context.Background(), nil)
	require.Equal(t, 2, retry.Synced,
		"the unchanged DAG source must retry every branch after a partial write")
	require.Zero(t, retry.Failed)
	fork, err = database.GetSession(t.Context(), "forked-i")
	require.NoError(t, err)
	require.NotNil(t, fork, "the retry must restore the missing fork")

	_, hasDigest, err = database.GetProviderStatHash(
		t.Context(), parser.AgentClaude, path,
	)
	require.NoError(t, err)
	assert.True(t, hasDigest,
		"the digest may persist after every branch commits")
}
