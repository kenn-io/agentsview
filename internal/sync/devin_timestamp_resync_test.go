package sync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// writeDevinMessageNodeOnlyFixture builds a Devin root whose session has no
// transcript artifact and no session-level timestamps. Both the session and its
// message timestamps then come solely from message_nodes.created_at, which is
// the shape whose fingerprint the epoch-seconds fix cannot disturb.
func writeDevinMessageNodeOnlyFixture(
	t *testing.T,
	root string,
	sessionID string,
	nodeCreatedAtSec int64,
) string {
	t.Helper()
	cliDir := filepath.Join(root, "cli")
	require.NoError(t, os.MkdirAll(cliDir, 0o755))
	dbPath := filepath.Join(cliDir, "sessions.db")

	database, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, database.Close()) }()

	_, err = database.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			title TEXT,
			working_directory TEXT,
			model TEXT,
			created_at INTEGER,
			last_activity_at INTEGER,
			main_chain_id INTEGER,
			hidden INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE message_nodes (
			row_id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			node_id INTEGER NOT NULL,
			parent_node_id INTEGER,
			chat_message TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			metadata TEXT
		);
	`)
	require.NoError(t, err)

	// created_at and last_activity_at stay NULL: this session's only timestamp
	// source is message_nodes.created_at.
	_, err = database.Exec(
		`INSERT INTO sessions (id, title, working_directory, model, hidden)
		 VALUES (?, ?, ?, ?, 0)`,
		sessionID, "Devin node-only fixture", "/src/agentsview", "devin-1",
	)
	require.NoError(t, err)

	for i, node := range []struct {
		nodeID  int64
		payload string
	}{
		{0, `{"role":"user","content":"Fix the parser"}`},
		{1, `{"role":"assistant","content":"Fixed it"}`},
	} {
		var parent any
		if i > 0 {
			parent = int64(i - 1)
		}
		_, err = database.Exec(
			`INSERT INTO message_nodes
				(session_id, node_id, parent_node_id, chat_message, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			sessionID, node.nodeID, parent, node.payload, nodeCreatedAtSec,
		)
		require.NoError(t, err)
	}
	return dbPath
}

// devinSourceFingerprint returns the provider fingerprint for the single Devin
// source under root, so a test can prove the fingerprint did or did not move.
func devinSourceFingerprint(t *testing.T, root string) parser.SourceFingerprint {
	t.Helper()
	var factory parser.ProviderFactory
	for _, candidate := range parser.ProviderFactories() {
		if candidate.Definition().Type == parser.AgentDevin {
			factory = candidate
			break
		}
	}
	require.NotNil(t, factory, "devin provider factory must be registered")

	provider := factory.NewProvider(parser.ProviderConfig{
		Roots: []string{root}, Machine: "local",
	})
	refs, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, refs, 1)

	fingerprint, err := provider.Fingerprint(context.Background(), refs[0])
	require.NoError(t, err)
	require.NotEmpty(t, fingerprint.Hash)
	return fingerprint
}

// staleStartedAt is what the pre-fix parser stored for a message-node session
// whose nodes carry created_at 1700000000: read as milliseconds that is
// 1970-01-20, which the parser kept only because it came from a message rather
// than the discarded session metadata.
const staleStartedAt = "1970-01-20T16:13:20Z"

// staleUserVersion picks the archive user_version to stamp: one below the
// current data version to demand a resync, or the current version to model an
// archive that a bump never reached.
func staleUserVersion(demandResync bool) int {
	if demandResync {
		return db.CurrentDataVersion() - 1
	}
	return db.CurrentDataVersion()
}

// writeStaleDevinTimestamps rewrites every stored session timestamp to the
// pre-fix value and stamps the archive's data version, standing in for an
// archive an older binary produced.
func writeStaleDevinTimestamps(t *testing.T, archivePath string, userVersion int) {
	t.Helper()
	conn, err := sql.Open("sqlite3", archivePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()

	_, err = conn.Exec(
		`UPDATE sessions SET started_at = ?, ended_at = ?`,
		staleStartedAt, staleStartedAt,
	)
	require.NoError(t, err)
	_, err = conn.Exec(fmt.Sprintf("PRAGMA user_version = %d", userVersion))
	require.NoError(t, err)
}

func requireOnlyStoredSession(t *testing.T, database *db.DB) db.Session {
	t.Helper()
	page, err := database.ListSessions(context.Background(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	return page.Sessions[0]
}

// TestDevinMessageNodeSessionResyncsDespiteUnchangedFingerprint protects the
// upgrade path for the epoch-seconds fix. A Devin session that falls back to
// message_nodes and has no usable sessions-row timestamps hashes to the same
// fingerprint before and after that fix, so nothing source-side signals that
// its stored 1970-era timestamps are wrong. Correction has to come from the
// stale-data-version resync, which rebuilds the archive and reparses every
// source unconditionally. If resync ever starts consulting fingerprints to skip
// unchanged sources, this session would keep its wrong timestamps forever and
// this test fails.
func TestDevinMessageNodeSessionResyncsDespiteUnchangedFingerprint(t *testing.T) {
	const (
		sessionID = "node-only-session"
		// 2023-11-14T22:13:20Z as epoch seconds. Read as milliseconds this is
		// 1970-01-20, which the parser discards as invalid.
		nodeCreatedAtSec = 1_700_000_000
		wantStartedAt    = "2023-11-14T22:13:20Z"
	)

	root := t.TempDir()
	writeDevinMessageNodeOnlyFixture(t, root, sessionID, nodeCreatedAtSec)
	archivePath := filepath.Join(t.TempDir(), "archive.db")

	database, err := db.Open(archivePath)
	require.NoError(t, err)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentDevin: {root}},
		Machine:   "local",
	})
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)

	stored := requireOnlyStoredSession(t, database)
	require.NotNil(t, stored.StartedAt)
	assert.Equal(t, wantStartedAt, *stored.StartedAt,
		"message_nodes.created_at is epoch seconds and must date the session")

	fingerprintBefore := devinSourceFingerprint(t, root)

	// Simulate the archive an older binary left behind: the same source, but
	// stored timestamps the millisecond misreading produced.
	engine.Close()
	require.NoError(t, database.Close())
	writeStaleDevinTimestamps(t, archivePath, staleUserVersion(false))

	// An incremental sync cannot repair this. The source bytes never changed,
	// so its fingerprint is what it always was, and the fix lives entirely in
	// how the parser interprets integers the fingerprint hashes raw.
	currentVersioned, err := db.Open(archivePath)
	require.NoError(t, err)
	require.False(t, currentVersioned.NeedsResync())
	incremental := NewEngine(currentVersioned, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentDevin: {root}},
		Machine:   "local",
	})
	incremental.SyncAll(context.Background(), nil)
	skipped := requireOnlyStoredSession(t, currentVersioned)
	require.NotNil(t, skipped.StartedAt)
	assert.Equal(t, staleStartedAt, *skipped.StartedAt,
		"incremental sync must be unable to notice the correction, which is why "+
			"this fix needs a data-version bump rather than a fingerprint change")
	assert.Equal(t, fingerprintBefore.Hash, devinSourceFingerprint(t, root).Hash,
		"the source is unchanged, so nothing source-side can signal the reparse")
	incremental.Close()
	require.NoError(t, currentVersioned.Close())

	// The data-version bump is the only thing that reaches this session.
	writeStaleDevinTimestamps(t, archivePath, staleUserVersion(true))
	reopened, err := db.Open(archivePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	require.True(t, reopened.NeedsResync(),
		"an archive below the current data version must ask for a resync")

	upgraded := NewEngine(reopened, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentDevin: {root}},
		Machine:   "local",
	})
	defer upgraded.Close()

	resyncStats := upgraded.ResyncAll(context.Background(), nil)
	require.False(t, resyncStats.Aborted)

	corrected := requireOnlyStoredSession(t, reopened)
	require.NotNil(t, corrected.StartedAt)
	assert.Equal(t, wantStartedAt, *corrected.StartedAt,
		"resync must reparse the session and replace the 1970-era timestamp")
	assert.False(t, reopened.NeedsResync(),
		"a completed resync must clear the stale-data flag")
}
