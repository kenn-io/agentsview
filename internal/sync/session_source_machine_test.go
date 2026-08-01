package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestMachineForPathUsesNormalizedRootSpecificity(t *testing.T) {
	parent := t.TempDir()
	nested := filepath.Join(parent, "nested")
	var legacyParent strings.Builder
	legacyParent.Grow(len(parent) + 20)
	legacyParent.WriteString(parent)
	for range 10 {
		legacyParent.WriteByte(byte(filepath.Separator))
		legacyParent.WriteByte('.')
	}
	legacyParentPath := legacyParent.String()
	require.Greater(t, len(legacyParentPath), len(nested))
	engine := &Engine{
		sourceMachines: map[parser.AgentType]map[string]string{
			parser.AgentClaude: {
				legacyParentPath: "parentbox",
				nested:           "nestedbox",
			},
		},
		machine: "localbox",
	}

	assert.Equal(t, "nestedbox", engine.machineForPath(
		parser.AgentClaude, filepath.Join(nested, "session.jsonl"),
	))
}

func TestReconcileWatchRootsTombstonesMissingLabeledFilesystemSession(
	t *testing.T,
) {
	root := t.TempDir()
	path := writeSessionSourceClaudeFile(t, root, "missing-session.jsonl")
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

	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(t, os.Remove(path))
	require.NoError(t, engine.ReconcileWatchRoots(
		t.Context(), []string{root}, false,
	))

	active, err := database.GetSession(t.Context(), "missing-session")
	require.NoError(t, err)
	assert.Nil(t, active)
	archived, err := database.GetSessionFull(t.Context(), "missing-session")
	require.NoError(t, err)
	require.NotNil(t, archived)
	assert.Equal(t, "archivebox", archived.Machine)
	require.NotNil(t, archived.DeletionCause)
	assert.Equal(t, "source_missing", *archived.DeletionCause)
}

func TestSyncAllSincePreservesIngestedFilesystemMachine(t *testing.T) {
	root := t.TempDir()
	writeSessionSourceClaudeFile(t, root, "ingested-machine.jsonl")
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
	require.Zero(t, second.Synced)

	sess, err := database.GetSessionFull(
		context.Background(), "ingested-machine",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "oldbox", sess.Machine)
	assert.Equal(t, 2, sess.MessageCount)
	assert.False(t, sess.LastWriteIncremental)
	snapshots, err := database.ListSessionProjectIdentitySnapshots(
		t.Context(),
	)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "oldbox", snapshots[0].Machine)
	observations, err := database.ListProjectIdentityObservations(
		t.Context(), []string{sess.Project},
	)
	require.NoError(t, err)
	require.Len(t, observations, 1)
	assert.Equal(t, "oldbox", observations[0].Machine)
}

func TestSyncAllSincePreservesTrashedSessionMachine(t *testing.T) {
	root := t.TempDir()
	writeSessionSourceClaudeFile(t, root, "trashed-machine.jsonl")
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

	require.Equal(t, 1, newEngine("oldbox").SyncAll(t.Context(), nil).Synced)
	require.NoError(t, database.SoftDeleteSession("trashed-machine"))

	stats := newEngine("newbox").SyncAllSince(
		t.Context(), time.Now().Add(time.Hour), nil,
	)
	require.Zero(t, stats.Synced)
	active, err := database.GetSession(t.Context(), "trashed-machine")
	require.NoError(t, err)
	assert.Nil(t, active)
	trashed, err := database.GetSessionFull(
		t.Context(), "trashed-machine",
	)
	require.NoError(t, err)
	require.NotNil(t, trashed)
	assert.Equal(t, "oldbox", trashed.Machine)
	assert.NotNil(t, trashed.DeletedAt)
	assert.Nil(t, trashed.DeletionCause)
}

func TestResyncAllPreservesSourceMachineIdentityAttribution(t *testing.T) {
	root := t.TempDir()
	writeSessionSourceClaudeFile(t, root, "resynced-identity.jsonl")
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

	require.Equal(t, 1, newEngine("oldbox").SyncAll(t.Context(), nil).Synced)
	stats := newEngine("newbox").ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)

	session, err := database.GetSessionFull(t.Context(), "resynced-identity")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "oldbox", session.Machine)
	snapshots, err := database.ListSessionProjectIdentitySnapshots(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "oldbox", snapshots[0].Machine)
	observations, err := database.ListProjectIdentityObservations(
		t.Context(), []string{session.Project},
	)
	require.NoError(t, err)
	require.Len(t, observations, 1)
	assert.Equal(t, "oldbox", observations[0].Machine)
}

func TestResyncAllPreservesTrashedSessionMachine(t *testing.T) {
	root := t.TempDir()
	writeSessionSourceClaudeFile(t, root, "resynced-trash.jsonl")
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

	require.Equal(t, 1, newEngine("oldbox").SyncAll(t.Context(), nil).Synced)
	require.NoError(t, database.SoftDeleteSession("resynced-trash"))
	stats := newEngine("newbox").ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)

	active, err := database.GetSession(t.Context(), "resynced-trash")
	require.NoError(t, err)
	assert.Nil(t, active)
	trashed, err := database.GetSessionFull(t.Context(), "resynced-trash")
	require.NoError(t, err)
	require.NotNil(t, trashed)
	assert.Equal(t, "oldbox", trashed.Machine)
	assert.NotNil(t, trashed.DeletedAt)
}

func TestIncrementalAppendPreservesIngestedSourceMachine(t *testing.T) {
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
	assert.Equal(t, "oldbox", sess.Machine)
	assert.Equal(t, 3, sess.MessageCount)
	assert.True(t, sess.LastWriteIncremental)
}

func TestFullReparsePreservesIngestedSourceMachine(t *testing.T) {
	root := t.TempDir()
	path := writeSessionSourceClaudeFile(t, root, "reparsed-machine.jsonl")
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

	require.Equal(t, 1, newEngine("oldbox").SyncAll(t.Context(), nil).Synced)
	builder := testjsonl.NewSessionBuilder()
	builder.AddClaudeUser("2026-07-01T10:01:00Z", "replacement")
	require.NoError(t, os.WriteFile(path, []byte(builder.String()), 0o600))

	require.Equal(t, 1, newEngine("newbox").SyncAll(t.Context(), nil).Synced)
	session, err := database.GetSessionFull(t.Context(), "reparsed-machine")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "oldbox", session.Machine)
	assert.Equal(t, 1, session.MessageCount)
	snapshots, err := database.ListSessionProjectIdentitySnapshots(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "oldbox", snapshots[0].Machine)
}

func TestIncompleteIncrementalAppendPreservesIngestedSourceMachine(t *testing.T) {
	root := t.TempDir()
	path := writeSessionSourceClaudeFile(t, root, "partial-machine.jsonl")
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

	require.Equal(t, 1, newEngine("oldbox").SyncAll(t.Context(), nil).Synced)
	before, err := database.GetSessionFull(t.Context(), "partial-machine")
	require.NoError(t, err)
	require.NotNil(t, before)

	completeLine := testjsonl.ClaudeUserJSON(
		"completed later", "2026-07-01T10:00:02Z",
	)
	partialAt := len(completeLine) / 2
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(completeLine[:partialAt])
	require.NoError(t, err)
	require.NoError(t, f.Close())

	second := newEngine("newbox").SyncAll(t.Context(), nil)
	require.Zero(t, second.Synced)
	after, err := database.GetSessionFull(t.Context(), "partial-machine")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, "oldbox", after.Machine)
	assert.Equal(t, before.FileSize, after.FileSize)
	assert.Equal(t, before.FileMtime, after.FileMtime)
	assert.Equal(t, before.FileHash, after.FileHash)
	assert.Equal(t, before.NextOrdinal, after.NextOrdinal)
	assert.Equal(t, before.LastEntryUUID, after.LastEntryUUID)
	assert.Equal(t, before.MessageCount, after.MessageCount)
	assert.Equal(t, before.LastWriteIncremental, after.LastWriteIncremental)

	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(completeLine[partialAt:] + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Equal(t, 1, newEngine("newbox").SyncAll(t.Context(), nil).Synced)

	completed, err := database.GetSessionFull(t.Context(), "partial-machine")
	require.NoError(t, err)
	require.NotNil(t, completed)
	assert.Equal(t, "oldbox", completed.Machine)
	assert.Equal(t, before.MessageCount+1, completed.MessageCount)
	assert.True(t, completed.LastWriteIncremental)
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
