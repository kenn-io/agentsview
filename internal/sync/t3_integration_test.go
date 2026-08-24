package sync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

// writeT3StateDB builds a t3code userdata database holding two threads in one
// project.
func writeT3StateDB(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	dbPath := filepath.Join(dir, "state.sqlite")
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()

	for _, stmt := range []string{
		`CREATE TABLE projection_projects (
			project_id TEXT PRIMARY KEY, title TEXT NOT NULL,
			workspace_root TEXT NOT NULL, deleted_at TEXT)`,
		`CREATE TABLE projection_threads (
			thread_id TEXT PRIMARY KEY, project_id TEXT NOT NULL,
			title TEXT NOT NULL, branch TEXT, worktree_path TEXT,
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			deleted_at TEXT, model_selection_json TEXT)`,
		`CREATE TABLE projection_thread_messages (
			message_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL,
			role TEXT NOT NULL, text TEXT NOT NULL,
			is_streaming INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			attachments_json TEXT)`,
		`CREATE TABLE projection_thread_sessions (
			thread_id TEXT PRIMARY KEY, status TEXT, provider_name TEXT,
			provider_instance_id TEXT, updated_at TEXT)`,
		`INSERT INTO projection_projects VALUES
			('p-1', 'acme-app', '/srv/code/acme-app', NULL)`,
		`INSERT INTO projection_threads VALUES
			('thread-alpha', 'p-1', 'Inspect the auth flow', 'main', NULL,
			 '2026-08-22T10:00:00.000Z', '2026-08-22T10:05:00.000Z', NULL,
			 '{"instanceId":"claudeAgent","model":"claude-sonnet-4-6"}')`,
		`INSERT INTO projection_threads VALUES
			('thread-beta', 'p-1', 'Rename the config loader', 'main', NULL,
			 '2026-08-22T11:00:00.000Z', '2026-08-22T11:02:00.000Z', NULL,
			 '{"provider":"codex","model":"gpt-5-codex"}')`,
		`INSERT INTO projection_thread_messages VALUES
			('m-a1', 'thread-alpha', 'user', 'Inspect the auth flow.', 0,
			 '2026-08-22T10:00:01.000Z', '2026-08-22T10:00:01.000Z', NULL)`,
		`INSERT INTO projection_thread_messages VALUES
			('m-a2', 'thread-alpha', 'assistant', 'The token refresh races.', 0,
			 '2026-08-22T10:05:00.000Z', '2026-08-22T10:05:00.000Z', NULL)`,
		`INSERT INTO projection_thread_messages VALUES
			('m-b1', 'thread-beta', 'user', 'Rename the config loader.', 0,
			 '2026-08-22T11:00:01.000Z', '2026-08-22T11:00:01.000Z', NULL)`,
	} {
		_, err := conn.Exec(stmt)
		require.NoError(t, err)
	}
	return dbPath
}

func TestSyncT3Threads(t *testing.T) {
	root := t.TempDir()
	userdata := filepath.Join(root, ".t3", "userdata")
	dbPath := writeT3StateDB(t, userdata)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentT3: {userdata},
		},
		Machine: "devbox",
	})

	// Discovery surfaces the one shared database; the parse fans it out into
	// two sessions.
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 2, Skipped: 0})

	ctx := context.Background()
	sess, err := database.GetSession(ctx, "t3:thread-alpha")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, "acme_app", sess.Project)
	// t3's own thread title reaches the display name rather than a first-message
	// fallback.
	require.NotNil(t, sess.DisplayName)
	assert.Equal(t, "Inspect the auth flow", *sess.DisplayName)
	// Each thread is stored under its own virtual member path.
	assert.Equal(t, dbPath+"#thread-alpha",
		database.GetSessionFilePath("t3:thread-alpha"))
	assert.Equal(t, dbPath+"#thread-beta",
		database.GetSessionFilePath("t3:thread-beta"))
	_, storedMtime, ok := database.GetSessionFileInfo("t3:thread-alpha")
	require.True(t, ok)
	assert.Equal(t, engine.SourceMtime("t3:thread-alpha"), storedMtime)

	msgs, err := database.GetMessages(ctx, "t3:thread-alpha", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "Inspect the auth flow.", msgs[0].Content)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "claude-sonnet-4-6", msgs[1].Model)

	// The container is still discovered, but its fingerprint is unchanged, so
	// no thread is reparsed.
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 0, Skipped: 0})
}

// The point of addressing threads individually is that a write to one of them
// does not drag the rest of the database through a reparse.
func TestSyncT3ReparsesOnlyTheChangedThread(t *testing.T) {
	root := t.TempDir()
	userdata := filepath.Join(root, ".t3", "userdata")
	dbPath := writeT3StateDB(t, userdata)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentT3: {userdata},
		},
		Machine: "devbox",
	})
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 2, Skipped: 0})

	ctx := context.Background()
	_, betaMtimeBefore, ok := database.GetSessionFileInfo("t3:thread-beta")
	require.True(t, ok)

	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(
		`INSERT INTO projection_thread_messages VALUES
		 ('m-a3', 'thread-alpha', 'user', 'Ship the fix.', 0,
		  '2026-08-22T12:00:00.000Z', '2026-08-22T12:00:00.000Z', NULL)`)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE projection_threads SET updated_at = '2026-08-22T12:00:00.000Z'
		  WHERE thread_id = 'thread-alpha'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1, Skipped: 0})

	alpha, err := database.GetSession(ctx, "t3:thread-alpha")
	require.NoError(t, err)
	require.NotNil(t, alpha)
	assert.Equal(t, 3, alpha.MessageCount)

	beta, err := database.GetSession(ctx, "t3:thread-beta")
	require.NoError(t, err)
	require.NotNil(t, beta)
	assert.Equal(t, 1, beta.MessageCount)
	_, betaMtimeAfter, ok := database.GetSessionFileInfo("t3:thread-beta")
	require.True(t, ok)
	assert.Equal(t, betaMtimeBefore, betaMtimeAfter,
		"an untouched thread keeps its checkpoint when a sibling changes")
}

// An in-place edit bumps only the message row's text and updated_at -- the
// thread row does not change. The edit must still be persisted: if the parsed
// mtime failed to advance with it, the engine would discard the reparse as
// unchanged and the stale text would survive.
func TestSyncT3PersistsInPlaceMessageEdit(t *testing.T) {
	root := t.TempDir()
	userdata := filepath.Join(root, ".t3", "userdata")
	dbPath := writeT3StateDB(t, userdata)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentT3: {userdata},
		},
		Machine: "devbox",
	})
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 2, Skipped: 0})

	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE projection_thread_messages
		    SET text = 'The token refresh races; pin the fixture clock.',
		        updated_at = '2026-08-22T13:00:00.000Z'
		  WHERE message_id = 'm-a2'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	// Only the edited thread is re-persisted; its sibling stays skipped.
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1, Skipped: 0})

	ctx := context.Background()
	msgs, err := database.GetMessages(ctx, "t3:thread-alpha", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "The token refresh races; pin the fixture clock.",
		msgs[1].Content)

	beta, err := database.GetSession(ctx, "t3:thread-beta")
	require.NoError(t, err)
	require.NotNil(t, beta)
	assert.Equal(t, 1, beta.MessageCount)

	// And the edit is not re-persisted forever: the stored mtime now matches
	// the change token, so the next pass skips everything.
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 0, Skipped: 0})
}

// A projection rebuild rewrites content while every event-derived timestamp
// stands still. Only the content digest can see it, so the rewrite must
// survive the mtime-and-hash unchanged gate and be persisted.
func TestSyncT3PersistsSameTimestampRewrite(t *testing.T) {
	root := t.TempDir()
	userdata := filepath.Join(root, ".t3", "userdata")
	dbPath := writeT3StateDB(t, userdata)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentT3: {userdata},
		},
		Machine: "devbox",
	})
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 2, Skipped: 0})

	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE projection_thread_messages
		    SET text = 'Refolded: the race was in the fixture clock.'
		  WHERE message_id = 'm-a2'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1, Skipped: 0})

	msgs, err := database.GetMessages(
		context.Background(), "t3:thread-alpha", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "Refolded: the race was in the fixture clock.",
		msgs[1].Content)

	// The stored hash now matches the digest, so the next pass converges.
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 0, Skipped: 0})
}

// The pre-parse freshness gate must consult the digest in both directions.
// A same-timestamp rewrite leaves size and mtime matching the stored row, so
// only the required hash comparison can defeat the skip; and another thread's
// growth moves the shared container's size under an unchanged member, where
// only the matching hash can establish freshness and keep reconciliation from
// reparsing the whole archive on every pass.
func TestT3FreshnessGateConsultsDigest(t *testing.T) {
	root := t.TempDir()
	userdata := filepath.Join(root, ".t3", "userdata")
	dbPath := writeT3StateDB(t, userdata)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentT3: {userdata},
		},
		Machine: "devbox",
	})
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 2, Skipped: 0})

	ctx := context.Background()
	provider, ok := parser.NewProvider(parser.AgentT3, parser.ProviderConfig{
		Roots: []string{userdata},
	})
	require.True(t, ok)
	factory, ok := parser.ProviderFactoryByType(parser.AgentT3)
	require.True(t, ok)
	semantics := factory.Capabilities().Sync
	require.True(t, semantics.FingerprintHashRequiredForFreshness)

	freshInDB := func(rawID string) bool {
		t.Helper()
		ref, found, err := provider.FindSource(ctx, parser.FindSourceRequest{
			RawSessionID:  rawID,
			FullSessionID: "t3:" + rawID,
		})
		require.NoError(t, err)
		require.True(t, found)
		fingerprint, err := provider.Fingerprint(ctx, ref)
		require.NoError(t, err)
		return engine.providerSourceUnchangedInDB(
			ctx, ref, fingerprint, semantics, nil,
		)
	}

	assert.True(t, freshInDB("thread-alpha"),
		"an untouched member is fresh against its stored row")

	// A rewrite that moves no timestamp and preserves length: size and mtime
	// both still match the stored row, so only the digest can see it.
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE projection_thread_messages
		    SET text = 'The token refresh RACES.'
		  WHERE message_id = 'm-a2'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	assert.False(t, freshInDB("thread-alpha"),
		"a timestamp-blind rewrite must defeat the freshness skip")

	// Restore and let the sync converge again before the growth case.
	conn, err = sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE projection_thread_messages
		    SET text = 'The token refresh races.'
		  WHERE message_id = 'm-a2'`)
	require.NoError(t, err)

	// Another thread's growth changes the shared container's size, which is
	// every member's fingerprint size. The unchanged member's matching digest
	// must still establish freshness, or reconciliation would reparse the
	// whole archive after every write anywhere in the database.
	sizeBefore, err := os.Stat(dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(
		`INSERT INTO projection_thread_messages VALUES
		 ('m-b2', 'thread-beta', 'assistant', ?, 0,
		  '2026-08-22T14:00:00.000Z', '2026-08-22T14:00:00.000Z', NULL)`,
		strings.Repeat("a reply long enough to force new database pages. ", 4096))
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	sizeAfter, err := os.Stat(dbPath)
	require.NoError(t, err)
	require.NotEqual(t, sizeBefore.Size(), sizeAfter.Size(),
		"the growth case only tests the escape hatch if the container size moved")

	assert.True(t, freshInDB("thread-alpha"),
		"an unchanged member stays fresh on its digest despite container growth")
	assert.False(t, freshInDB("thread-beta"),
		"the thread that actually changed still reparses")
}

// A soft-deleted thread stops being discovered.
func TestSyncT3SkipsSoftDeletedThread(t *testing.T) {
	root := t.TempDir()
	userdata := filepath.Join(root, ".t3", "userdata")
	dbPath := writeT3StateDB(t, userdata)
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE projection_threads SET deleted_at = '2026-08-22T11:30:00.000Z'
		  WHERE thread_id = 'thread-beta'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentT3: {userdata},
		},
		Machine: "devbox",
	})
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1, Skipped: 0})

	sess, err := database.GetSession(context.Background(), "t3:thread-beta")
	require.NoError(t, err)
	assert.Nil(t, sess)
}
