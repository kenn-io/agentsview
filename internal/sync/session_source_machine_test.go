package sync

import (
	"database/sql"
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
	assert := assert.New(t)
	require := require.New(t)

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

	stats := engine.SyncAll(t.Context(), nil)

	assert.False(stats.Aborted)
	page, err := database.ListSessions(t.Context(), db.SessionFilter{
		Limit: 10,
	})
	require.NoError(err)
	require.Len(page.Sessions, 2)
	machines := map[string]string{}
	for _, sess := range page.Sessions {
		machines[sess.ID] = sess.Machine
	}
	assert.Equal("localbox", machines["local-session"])
	assert.Equal("archivebox", machines["archive-session"])
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

	engine.SyncPathsContext(t.Context(), []string{path})

	sess, err := database.GetSessionFull(t.Context(), "watched-session")
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

func TestMachineForPathMatchesAbsolutePathToRelativeRoot(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)
	relativeRoot := filepath.Join("testdata", "relative-session-source")
	engine := &Engine{
		sourceMachines: map[parser.AgentType]map[string]string{
			parser.AgentClaude: {relativeRoot: "archivebox"},
		},
		machine: "localbox",
	}

	assert.Equal(t, "archivebox", engine.machineForPath(
		parser.AgentClaude,
		filepath.Join(cwd, relativeRoot, "session.jsonl"),
	))
}

func TestReconcileWatchRootsTombstonesMissingLabeledFilesystemSession(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

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

	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(os.Remove(path))
	require.NoError(engine.ReconcileWatchRoots(
		t.Context(), []string{root}, false,
	))

	active, err := database.GetSession(t.Context(), "missing-session")
	require.NoError(err)
	assert.NotNil(active)
	archived, err := database.GetSessionFull(t.Context(), "missing-session")
	require.NoError(err)
	assertSourceMissingState(t, archived)
	assert.Equal("archivebox", archived.Machine)
}

func TestSyncAllSincePreservesIngestedFilesystemMachine(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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

	first := newEngine("oldbox").SyncAll(t.Context(), nil)
	require.Equal(1, first.Synced)
	second := newEngine("newbox").SyncAllSince(
		t.Context(), time.Now().Add(time.Hour), nil,
	)
	require.Zero(second.Synced)

	sess, err := database.GetSessionFull(
		t.Context(), "ingested-machine",
	)
	require.NoError(err)
	require.NotNil(sess)
	assert.Equal("oldbox", sess.Machine)
	assert.Equal(2, sess.MessageCount)
	assert.False(sess.LastWriteIncremental)
	snapshots, err := database.ListSessionProjectIdentitySnapshots(
		t.Context(),
	)
	require.NoError(err)
	require.Len(snapshots, 1)
	assert.Equal("oldbox", snapshots[0].Machine)
	observations, err := database.ListProjectIdentityObservations(
		t.Context(), []string{sess.Project},
	)
	require.NoError(err)
	require.Len(observations, 1)
	assert.Equal("oldbox", observations[0].Machine)
}

func TestSyncAllSincePreservesTrashedSessionMachine(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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

	require.Equal(1, newEngine("oldbox").SyncAll(t.Context(), nil).Synced)
	require.NoError(database.SoftDeleteSession("trashed-machine"))

	stats := newEngine("newbox").SyncAllSince(
		t.Context(), time.Now().Add(time.Hour), nil,
	)
	require.Zero(stats.Synced)
	active, err := database.GetSession(t.Context(), "trashed-machine")
	require.NoError(err)
	assert.Nil(active)
	trashed, err := database.GetSessionFull(
		t.Context(), "trashed-machine",
	)
	require.NoError(err)
	require.NotNil(trashed)
	assert.Equal("oldbox", trashed.Machine)
	assert.NotNil(trashed.DeletedAt)
	assert.Nil(trashed.DeletionCause)
}

func TestResyncAllPreservesSourceMachineIdentityAttribution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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

	require.Equal(1, newEngine("oldbox").SyncAll(t.Context(), nil).Synced)
	stats := newEngine("newbox").ResyncAll(t.Context(), nil)
	require.False(stats.Aborted)

	session, err := database.GetSessionFull(t.Context(), "resynced-identity")
	require.NoError(err)
	require.NotNil(session)
	assert.Equal("oldbox", session.Machine)
	snapshots, err := database.ListSessionProjectIdentitySnapshots(t.Context())
	require.NoError(err)
	require.Len(snapshots, 1)
	assert.Equal("oldbox", snapshots[0].Machine)
	observations, err := database.ListProjectIdentityObservations(
		t.Context(), []string{session.Project},
	)
	require.NoError(err)
	require.Len(observations, 1)
	assert.Equal("oldbox", observations[0].Machine)
}

func TestResyncAllPreservesTrashedSessionMachine(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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

	require.Equal(1, newEngine("oldbox").SyncAll(t.Context(), nil).Synced)
	require.NoError(database.SoftDeleteSession("resynced-trash"))
	stats := newEngine("newbox").ResyncAll(t.Context(), nil)
	require.False(stats.Aborted)

	active, err := database.GetSession(t.Context(), "resynced-trash")
	require.NoError(err)
	assert.Nil(active)
	trashed, err := database.GetSessionFull(t.Context(), "resynced-trash")
	require.NoError(err)
	require.NotNil(trashed)
	assert.Equal("oldbox", trashed.Machine)
	assert.NotNil(trashed.DeletedAt)
}

func TestIncrementalAppendPreservesIngestedSourceMachine(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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

	first := newEngine("oldbox").SyncAll(t.Context(), nil)
	require.Equal(1, first.Synced)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(err)
	_, err = f.WriteString(testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON(
			"appended message", "2026-07-01T10:00:02Z",
		),
	))
	require.NoError(err)
	require.NoError(f.Close())

	second := newEngine("newbox").SyncAll(t.Context(), nil)
	require.Equal(1, second.Synced)

	sess, err := database.GetSessionFull(
		t.Context(), "incremental-machine",
	)
	require.NoError(err)
	require.NotNil(sess)
	assert.Equal("oldbox", sess.Machine)
	assert.Equal(3, sess.MessageCount)
	assert.True(sess.LastWriteIncremental)
}

func TestFullReparsePreservesIngestedSourceMachine(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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

	require.Equal(1, newEngine("oldbox").SyncAll(t.Context(), nil).Synced)
	builder := testjsonl.NewSessionBuilder()
	builder.AddClaudeUser("2026-07-01T10:01:00Z", "replacement")
	require.NoError(os.WriteFile(path, []byte(builder.String()), 0o600))

	require.Equal(1, newEngine("newbox").SyncAll(t.Context(), nil).Synced)
	session, err := database.GetSessionFull(t.Context(), "reparsed-machine")
	require.NoError(err)
	require.NotNil(session)
	assert.Equal("oldbox", session.Machine)
	assert.Equal(1, session.MessageCount)
	snapshots, err := database.ListSessionProjectIdentitySnapshots(t.Context())
	require.NoError(err)
	require.Len(snapshots, 1)
	assert.Equal("oldbox", snapshots[0].Machine)
}

func TestIncompleteIncrementalAppendPreservesIngestedSourceMachine(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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

	require.Equal(1, newEngine("oldbox").SyncAll(t.Context(), nil).Synced)
	before, err := database.GetSessionFull(t.Context(), "partial-machine")
	require.NoError(err)
	require.NotNil(before)

	completeLine := testjsonl.ClaudeUserJSON(
		"completed later", "2026-07-01T10:00:02Z",
	)
	partialAt := len(completeLine) / 2
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(err)
	_, err = f.WriteString(completeLine[:partialAt])
	require.NoError(err)
	require.NoError(f.Close())

	second := newEngine("newbox").SyncAll(t.Context(), nil)
	require.Zero(second.Synced)
	after, err := database.GetSessionFull(t.Context(), "partial-machine")
	require.NoError(err)
	require.NotNil(after)
	assert.Equal("oldbox", after.Machine)
	assert.Equal(before.FileSize, after.FileSize)
	assert.Equal(before.FileMtime, after.FileMtime)
	assert.Equal(before.FileHash, after.FileHash)
	assert.Equal(before.NextOrdinal, after.NextOrdinal)
	assert.Equal(before.LastEntryUUID, after.LastEntryUUID)
	assert.Equal(before.MessageCount, after.MessageCount)
	assert.Equal(before.LastWriteIncremental, after.LastWriteIncremental)

	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(err)
	_, err = f.WriteString(completeLine[partialAt:] + "\n")
	require.NoError(err)
	require.NoError(f.Close())
	require.Equal(1, newEngine("newbox").SyncAll(t.Context(), nil).Synced)

	completed, err := database.GetSessionFull(t.Context(), "partial-machine")
	require.NoError(err)
	require.NotNil(completed)
	assert.Equal("oldbox", completed.Machine)
	assert.Equal(before.MessageCount+1, completed.MessageCount)
	assert.True(completed.LastWriteIncremental)
}

func TestCopiedFilesystemSessionKeepsNativeIDDeduplication(t *testing.T) {
	require := require.New(t)

	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	firstPath := writeSessionSourceClaudeFile(t, firstRoot, "copied-session.jsonl")
	secondProject := filepath.Join(secondRoot, "project")
	require.NoError(os.MkdirAll(secondProject, 0o755))
	data, err := os.ReadFile(firstPath)
	require.NoError(err)
	require.NoError(os.WriteFile(
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

	engine.SyncAll(t.Context(), nil)

	page, err := database.ListSessions(t.Context(), db.SessionFilter{
		Limit: 10,
	})
	require.NoError(err)
	require.Len(page.Sessions, 1)
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

// TestReconcileTombstonesAfterSourceLabelChange pins the deletion path across a
// configuration edit. Attribution is immutable, so a session admitted under the
// old label keeps it; reconciliation must therefore query stored attribution
// rather than the currently configured label, or the delete is never noticed.
func TestReconcileTombstonesAfterSourceLabelChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	archiveRoot := t.TempDir()
	archivePath := writeSessionSourceClaudeFile(t, archiveRoot, "archive-session.jsonl")
	database := openTestDB(t)

	newEngine := func(machine string) *Engine {
		return NewEngine(database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentClaude: {archiveRoot},
			},
			SourceMachines: map[parser.AgentType]map[string]string{
				parser.AgentClaude: {archiveRoot: machine},
			},
			Machine: "localbox",
		})
	}

	first := newEngine("archivebox")
	t.Cleanup(first.Close)
	require.False(first.SyncAll(t.Context(), nil).Aborted)

	require.Equal("archivebox", activeSessionMachines(t, database)["archive-session"])
	// Model an archive admitted before deletion-proof baselines existed. The
	// relabeled reconciliation must recreate proof under the stored machine,
	// not only visit the configured candidate machine.
	require.NoError(database.Update(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			"DELETE FROM local_session_source_baselines WHERE session_id = ?",
			"archive-session",
		)
		return err
	}))
	appendSessionSourceClaudeMessage(t, archivePath)

	// The user edits the label. Existing rows keep "archivebox" by design.
	relabeled := newEngine("renamedbox")
	t.Cleanup(relabeled.Close)
	require.NoError(relabeled.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{archiveRoot}, false,
	))
	assert.Equal("archivebox", activeSessionMachines(t, database)["archive-session"],
		"an edited label must not rewrite an already-ingested session")
	ownership, err := database.ListActiveSessionSourceOwnershipScopesPage(
		t.Context(), "archivebox", string(parser.AgentClaude),
		[]db.StoredSourcePathHintScope{{Path: archiveRoot}},
		db.SessionSourceCursor{},
	)
	require.NoError(err)
	require.Len(ownership, 1,
		"reconciliation must restore deletion proof under stored attribution")

	// Now delete the source and reconcile under the new label.
	require.NoError(os.Remove(archivePath))
	require.NoError(relabeled.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{archiveRoot}, false,
	))

	archived, err := database.GetSessionFull(t.Context(), "archive-session")
	require.NoError(err)
	assertSourceMissingState(t, archived)
	assert.Equal("archivebox", archived.Machine)
}

func TestReconcileTombstonesLegacyEmptyMachineSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := writeSessionSourceClaudeFile(t, root, "legacy-empty-machine.jsonl")
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
	t.Cleanup(engine.Close)
	require.False(engine.SyncAll(t.Context(), nil).Aborted)

	// Model a session admitted before machine attribution and deletion-proof
	// baselines existed. Refreshing it must retain the empty attribution while
	// recreating deletion proof for that exact stored ownership key.
	require.NoError(database.Update(func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			"UPDATE sessions SET machine = '' WHERE id = ?",
			"legacy-empty-machine",
		); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(),
			"DELETE FROM local_session_source_baselines WHERE session_id = ?",
			"legacy-empty-machine",
		)
		return err
	}))
	appendSessionSourceClaudeMessage(t, path)
	require.NoError(engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{root}, false,
	))
	machine, exists := activeSessionMachines(t, database)["legacy-empty-machine"]
	require.True(exists)
	assert.Empty(machine)
	ownership, err := database.ListActiveSessionSourceOwnershipScopesPage(
		t.Context(), "", string(parser.AgentClaude),
		[]db.StoredSourcePathHintScope{{Path: root}},
		db.SessionSourceCursor{},
	)
	require.NoError(err)
	require.Len(ownership, 1,
		"refresh must restore deletion proof for the empty stored machine key")

	require.NoError(os.Remove(path))
	require.NoError(engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{root}, false,
	))

	active, err := database.GetSession(t.Context(), "legacy-empty-machine")
	require.NoError(err)
	assert.NotNil(active)
	archived, err := database.GetSessionFull(t.Context(), "legacy-empty-machine")
	require.NoError(err)
	assertSourceMissingState(t, archived)
	assert.Empty(archived.Machine)
}

// activeSessionMachines returns the stored machine of every active session,
// keyed by session ID.
func activeSessionMachines(t *testing.T, database *db.DB) map[string]string {
	t.Helper()
	page, err := database.ListSessions(t.Context(), db.SessionFilter{
		Limit: 100,
	})
	require.NoError(t, err)
	out := make(map[string]string, len(page.Sessions))
	for _, session := range page.Sessions {
		out[session.ID] = session.Machine
	}
	return out
}

// TestBaselineFollowsPersistedMachineAfterRelabel pins the ownership baseline
// to the machine a session was actually written under. prepareSessionWrite
// preserves the original label, so keying the baseline off the freshly parsed
// (configured) machine strands it under a machine no session row holds, and the
// source can never be tombstoned once it disappears.
func TestBaselineFollowsPersistedMachineAfterRelabel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	archiveRoot := t.TempDir()
	archivePath := writeSessionSourceClaudeFile(t, archiveRoot, "archive-session.jsonl")
	database := openTestDB(t)

	newEngine := func(machine string) *Engine {
		return NewEngine(database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentClaude: {archiveRoot},
			},
			SourceMachines: map[parser.AgentType]map[string]string{
				parser.AgentClaude: {archiveRoot: machine},
			},
			Machine: "localbox",
		})
	}

	first := newEngine("archivebox")
	t.Cleanup(first.Close)
	require.False(first.SyncAll(t.Context(), nil).Aborted)

	// Append to the source so the relabeled pass actually reparses and rewrites
	// it. An unchanged file is skipped, which never exercises the write path.
	appendSessionSourceClaudeMessage(t, archivePath)

	// Relabel the root and resync. The session keeps "archivebox"; the baseline
	// must land there too, not under the newly configured "renamedbox".
	relabeled := newEngine("renamedbox")
	t.Cleanup(relabeled.Close)
	require.False(relabeled.SyncAll(t.Context(), nil).Aborted)

	require.Equal("archivebox",
		activeSessionMachines(t, database)["archive-session"])

	ownershipFor := func(machine string) []db.SessionSourceOwnership {
		rows, err := database.ListActiveSessionSourceOwnershipScopesPage(
			t.Context(), machine, string(parser.AgentClaude),
			[]db.StoredSourcePathHintScope{{Path: archiveRoot}},
			db.SessionSourceCursor{},
		)
		require.NoError(err)
		return rows
	}

	stranded := ownershipFor("renamedbox")
	assert.Empty(stranded,
		"the baseline must not be keyed under a label no session row holds")

	owned := ownershipFor("archivebox")
	require.Len(owned, 1,
		"the baseline must follow the persisted machine")
	assert.Equal(archivePath, owned[0].FilePath)
}

// appendSessionSourceClaudeMessage grows an existing Claude transcript so the
// next sync sees a changed source instead of skipping it.
func appendSessionSourceClaudeMessage(t *testing.T, path string) {
	t.Helper()
	builder := testjsonl.NewSessionBuilder()
	builder.AddClaudeUser("2026-07-01T10:00:00Z", "hello")
	builder.AddClaudeAssistant("2026-07-01T10:00:01Z", "hi")
	builder.AddClaudeUser("2026-07-01T10:00:02Z", "more")
	builder.AddClaudeAssistant("2026-07-01T10:00:03Z", "sure")
	require.NoError(t, os.WriteFile(path, []byte(builder.String()), 0o600))
}
