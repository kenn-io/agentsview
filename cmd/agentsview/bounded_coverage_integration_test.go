package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

// TestBoundedCoverageCoordinatorCardinality uses the production coordinator,
// Engine resolver, journal drain, and source-application seam. Only the
// journal's changed row is observed for both archive cardinalities.
func TestBoundedCoverageCoordinatorCardinality(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping bounded coverage cardinality integration")
	}
	for _, mode := range []struct {
		name   string
		native bool
	}{
		{name: "native", native: true},
		{name: "degraded", native: false},
	} {
		for _, sessions := range []int{10, 5000} {
			t.Run(fmt.Sprintf("%s_sessions_%d", mode.name, sessions), func(t *testing.T) {
				root := t.TempDir()
				dbPath := filepath.Join(root, "opencode.db")
				journal, err := sql.Open("sqlite3", dbPath)
				require.NoError(t, err)
				t.Cleanup(func() { _ = journal.Close() })
				_, err = journal.Exec("PRAGMA wal_autocheckpoint=0")
				require.NoError(t, err)
				_, err = journal.Exec(boundedCoverageFixtureSchema)
				require.NoError(t, err)
				var journalMode string
				err = journal.QueryRow("PRAGMA journal_mode=WAL").Scan(&journalMode)
				require.NoError(t, err)
				require.Equal(t, "wal", journalMode)
				_, err = journal.Exec("INSERT INTO project (id, worktree, time_updated) VALUES ('proj', ?, 1)", root)
				require.NoError(t, err)
				for i := range sessions {
					id := fmt.Sprintf("ses%05d", i)
					_, err = journal.Exec(
						"INSERT INTO session (id, project_id, time_created, time_updated) VALUES (?, 'proj', 1, 1)", id,
					)
					require.NoError(t, err)
				}
				_, err = journal.Exec(`INSERT INTO message
			(id, session_id, data, time_created, time_updated)
			VALUES ('msg-0', 'ses00000', '{"role":"assistant"}', 1, 1)`)
				require.NoError(t, err)
				_, err = journal.Exec(`INSERT INTO part
			(id, session_id, message_id, data, time_created, time_updated)
			VALUES ('part-0', 'ses00000', 'msg-0',
			'{"type":"text","content":"changed"}', 1, 1)`)
				require.NoError(t, err)
				archive := dbtest.OpenTestDB(t)
				engine := agentsync.NewEngine(archive, agentsync.EngineConfig{
					AgentDirs: map[parser.AgentType][]string{parser.AgentOpenCode: {root}},
					Machine:   "local",
				})
				t.Cleanup(engine.Close)
				coordinator := &sharedUnwatchedPollCoordinator{
					ctx: context.Background(), coverage: engine,
					coverageState: make(map[string]*boundedCoverageState),
				}
				var rows, applied int
				coordinator.onBoundedCoveragePage = func(result parser.OpenCodeFeedResult) {
					rows += result.RowsRead
				}
				coordinator.onBoundedCoverageApply = func(stats agentsync.SyncStats) {
					applied += stats.Synced
				}
				roots := []agentsync.BoundedCoverageRoot{{Agent: parser.AgentOpenCode, Root: root}}
				bindings, err := engine.BoundedCoverageBindings(t.Context(), roots)
				require.NoError(t, err)
				eventTypes := []string{"message.updated", "message.part.updated", "session.updated"}
				for i, eventType := range eventTypes {
					payload := "{}"
					if eventType == "message.updated" {
						payload = `{"sessionID":"ses00000","info":{"id":"msg-0","sessionID":"ses00000","role":"user"}}`
					}
					_, err = journal.Exec(`INSERT INTO event
					(id, aggregate_id, seq, type, data)
					VALUES (?, 'ses00000', ?, ?, ?)`, fmt.Sprintf("event-before-%d", i), i+1, eventType, payload)
					require.NoError(t, err)
				}
				_, err = coordinator.AdmitBoundedCoverage(t.Context(), bindings, mode.native)
				require.NoError(t, err)
				for i, eventType := range eventTypes {
					payload := "{}"
					if eventType == "message.updated" {
						payload = `{"sessionID":"ses00000","info":{"id":"msg-0","sessionID":"ses00000","role":"user"}}`
					}
					_, err = journal.Exec(`INSERT INTO event
					(id, aggregate_id, seq, type, data)
					VALUES (?, 'ses00000', ?, ?, ?)`, fmt.Sprintf("event-after-%d", i), i+4, eventType, payload)
					require.NoError(t, err)
				}
				walInfo, err := os.Stat(dbPath + "-wal")
				require.NoError(t, err)
				require.Greater(t, walInfo.Size(), int64(32),
					"the measured mutation must retain WAL frames beyond its header")
				require.NoError(t, coordinator.refreshBoundedCoverage(map[string]pollingObligation{
					"degraded": {Key: "degraded", Scopes: []pollingScope{{Agent: parser.AgentOpenCode, Root: root}}},
				}))
				require.NoError(t, coordinator.pollBoundedCoverageOnce(t.Context()))
				_, err = archive.GetSession(t.Context(), "ses00000")
				require.NoError(t, err)
				t.Logf("bounded_admission mode=%s sessions=%d event_types=%s observed_journal_rows=%d applied_sources=%d source=%s wal_bytes=%d", mode.name, sessions, strings.Join(eventTypes, ","), rows, applied, bindings[0].PhysicalDBPath, walInfo.Size())
				require.GreaterOrEqual(t, rows, 2, "row-zero admission must retain pre-admission and triggering rows")
				require.Equal(t, 1, applied)
				require.LessOrEqual(t, rows, parser.OpenCodeCoverageMaxRows)
			})
		}
	}
}

func TestBoundedCoverageLeaseRejectsReplacedDatabase(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "opencode.db")
	journal, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = journal.Exec(boundedCoverageFixtureSchema)
	require.NoError(t, err)
	require.NoError(t, journal.Close())

	archive := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(archive, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentOpenCode: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	bindings, err := engine.BoundedCoverageBindings(t.Context(), []agentsync.BoundedCoverageRoot{{Agent: parser.AgentOpenCode, Root: root}})
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	bindings[0].Generation = 1
	lease, err := engine.AdmitBoundedCoverageLease(t.Context(), bindings[0])
	require.NoError(t, err)
	_, err = engine.TransitionBoundedCoverageRequest(t.Context(), lease, nil, lease.AdmissionCheckpoint, true)
	require.NoError(t, err)

	backup := filepath.Join(root, "opencode.old.db")
	require.NoError(t, os.Rename(dbPath, backup))
	replacement, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = replacement.Exec(boundedCoverageFixtureSchema)
	require.NoError(t, err)
	require.NoError(t, replacement.Close())

	_, err = engine.TransitionBoundedCoverageRequest(t.Context(), lease, nil, lease.AdmissionCheckpoint, false)
	require.Error(t, err, "replacement must invalidate the old physical lease before commit")
}

func TestBoundedCoverageBindingsDeduplicateSymlinkedRoots(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "opencode.db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(boundedCoverageFixtureSchema)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}

	archive := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(archive, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentOpenCode: {root, alias}},
		Machine:   "local",
	})
	defer engine.Close()
	bindings, err := engine.BoundedCoverageBindings(t.Context(), []agentsync.BoundedCoverageRoot{
		{Agent: parser.AgentOpenCode, Root: root},
		{Agent: parser.AgentOpenCode, Root: alias},
	})
	require.NoError(t, err)
	require.Len(t, bindings, 1,
		"lexical aliases of one physical database must share one coverage binding")
	assert.Equal(t, filepath.Clean(dbPath), bindings[0].DBPath)
	assert.Equal(t, filepath.Clean(root), bindings[0].Scope)
}

const boundedCoverageFixtureSchema = `
CREATE TABLE project (id TEXT PRIMARY KEY, worktree TEXT NOT NULL, time_updated INTEGER NOT NULL);
CREATE TABLE session (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, parent_id TEXT, title TEXT, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL);
CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, data TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL);
CREATE TABLE part (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, message_id TEXT NOT NULL, data TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL);
CREATE INDEX message_session_time_created_id_idx ON message (session_id, time_created, id);
CREATE INDEX part_session_idx ON part (session_id);
CREATE INDEX part_message_id_id_idx ON part (message_id, id);
CREATE TABLE event (id TEXT NOT NULL PRIMARY KEY, aggregate_id TEXT NOT NULL, seq INTEGER NOT NULL, type TEXT NOT NULL, data BLOB NOT NULL);
CREATE TABLE event_sequence (id TEXT NOT NULL PRIMARY KEY, owner_id TEXT);
`
