package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestSyncAllAttributesFilesystemSessionsPerRoot(t *testing.T) {
	localRoot := t.TempDir()
	archiveRoot := t.TempDir()
	writeSessionSourceClaudeFile(t, localRoot, "local-session.jsonl")
	writeSessionSourceClaudeFile(t, archiveRoot, "archive-session.jsonl")
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {localRoot, archiveRoot},
		},
		SourceMachines: map[parser.AgentType]map[string]string{
			parser.AgentClaude: {
				localRoot:   "localbox",
				archiveRoot: "archivebox",
			},
		},
		Machine: "localbox",
	})

	stats := engine.SyncAll(context.Background(), nil)

	assert.False(t, stats.Aborted)
	page, err := database.ListSessions(context.Background(), db.SessionFilter{
		Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 2)
	machines := map[string]string{}
	for _, sess := range page.Sessions {
		machines[sess.ID] = sess.Machine
	}
	assert.Equal(t, "localbox", machines["local-session"])
	assert.Equal(t, "archivebox", machines["archive-session"])
}

func TestSyncPathsAttributesFilesystemSessionFromChangedRoot(t *testing.T) {
	root := t.TempDir()
	path := writeSessionSourceClaudeFile(t, root, "watched-session.jsonl")
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		SourceMachines: map[parser.AgentType]map[string]string{
			parser.AgentClaude: {root: "archivebox"},
		},
		Machine: "localbox",
	})

	engine.SyncPathsContext(context.Background(), []string{path})

	sess, err := database.GetSessionFull(context.Background(), "watched-session")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "archivebox", sess.Machine)
}

func TestSyncAllSinceReattributesUnchangedFilesystemSession(t *testing.T) {
	root := t.TempDir()
	writeSessionSourceClaudeFile(t, root, "reattributed-session.jsonl")
	database := openTestDB(t)
	newEngine := func(machine string) *Engine {
		return NewEngine(database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentClaude: {root},
			},
			SourceMachines: map[parser.AgentType]map[string]string{
				parser.AgentClaude: {root: machine},
			},
			Machine: "localbox",
		})
	}

	first := newEngine("oldbox").SyncAll(context.Background(), nil)
	require.Equal(t, 1, first.Synced)
	second := newEngine("newbox").SyncAllSince(
		context.Background(), time.Now().Add(time.Hour), nil,
	)
	require.Equal(t, 1, second.Synced)

	sess, err := database.GetSessionFull(
		context.Background(), "reattributed-session",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "newbox", sess.Machine)
	assert.Equal(t, 2, sess.MessageCount)
	assert.False(t, sess.LastWriteIncremental)
}

func TestIncrementalAppendUsesCurrentSourceMachine(t *testing.T) {
	root := t.TempDir()
	path := writeSessionSourceClaudeFile(t, root, "incremental-machine.jsonl")
	database := openTestDB(t)
	newEngine := func(machine string) *Engine {
		return NewEngine(database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentClaude: {root},
			},
			SourceMachines: map[parser.AgentType]map[string]string{
				parser.AgentClaude: {root: machine},
			},
			Machine: "localbox",
		})
	}

	first := newEngine("oldbox").SyncAll(context.Background(), nil)
	require.Equal(t, 1, first.Synced)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON(
			"appended message", "2026-07-01T10:00:02Z",
		),
	))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	second := newEngine("newbox").SyncAll(context.Background(), nil)
	require.Equal(t, 1, second.Synced)

	sess, err := database.GetSessionFull(
		context.Background(), "incremental-machine",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "newbox", sess.Machine)
	assert.Equal(t, 3, sess.MessageCount)
	assert.True(t, sess.LastWriteIncremental)
}

func TestCopiedFilesystemSessionKeepsNativeIDDeduplication(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	firstPath := writeSessionSourceClaudeFile(t, firstRoot, "copied-session.jsonl")
	secondProject := filepath.Join(secondRoot, "project")
	require.NoError(t, os.MkdirAll(secondProject, 0o755))
	data, err := os.ReadFile(firstPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(secondProject, "copied-session.jsonl"), data, 0o600,
	))
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {firstRoot, secondRoot},
		},
		SourceMachines: map[parser.AgentType]map[string]string{
			parser.AgentClaude: {
				firstRoot:  "firstbox",
				secondRoot: "secondbox",
			},
		},
		Machine: "localbox",
	})

	engine.SyncAll(context.Background(), nil)

	page, err := database.ListSessions(context.Background(), db.SessionFilter{
		Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "copied-session", page.Sessions[0].ID)
}

func writeSessionSourceClaudeFile(t *testing.T, root, name string) string {
	t.Helper()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	builder := testjsonl.NewSessionBuilder()
	builder.AddClaudeUser("2026-07-01T10:00:00Z", "hello")
	builder.AddClaudeAssistant("2026-07-01T10:00:01Z", "hi")
	path := filepath.Join(project, name)
	require.NoError(t, os.WriteFile(path, []byte(builder.String()), 0o600))
	return path
}
