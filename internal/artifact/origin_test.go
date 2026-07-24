package artifact

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestEnsureOriginPersists(t *testing.T) {
	database := testDB(t)

	first, err := EnsureOrigin(database)
	require.NoError(t, err)
	require.NotEmpty(t, first)
	require.NotEqual(t, "local", first)

	second, err := EnsureOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestAdoptOriginPersistsConfigOrigin(t *testing.T) {
	database := testDB(t)

	require.NoError(t, AdoptOrigin(database, "desk-a1b2c3"))

	stored, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, "desk-a1b2c3", stored)

	// EnsureOrigin and its callers now agree with the adopted origin instead
	// of generating a divergent DB-only value.
	ensured, err := EnsureOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, "desk-a1b2c3", ensured)
}

func TestAdoptOriginIsIdempotent(t *testing.T) {
	database := testDB(t)

	require.NoError(t, AdoptOrigin(database, "desk-a1b2c3"))
	require.NoError(t, AdoptOrigin(database, "desk-a1b2c3"))

	stored, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, "desk-a1b2c3", stored)
}

func TestAdoptOriginOverwritesDivergentDBOrigin(t *testing.T) {
	database := testDB(t)

	// Simulate the pre-fix state: the recorder generated a DB-only origin
	// before the authoritative config origin existed.
	stale, err := EnsureOrigin(database)
	require.NoError(t, err)
	require.NotEqual(t, "desk-a1b2c3", stale)

	require.NoError(t, AdoptOrigin(database, "desk-a1b2c3"))

	stored, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, "desk-a1b2c3", stored)
}

func TestAdoptOriginRejectsInvalidOrigin(t *testing.T) {
	database := testDB(t)

	err := AdoptOrigin(database, "../outside")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "adopting artifact origin")

	stored, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Empty(t, stored)
}

func TestEnsureOriginRejectsInvalidPersistedOrigin(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.SetSyncState(originStateKey, "../outside"))

	origin, err := EnsureOrigin(database)
	require.Error(t, err)
	assert.Empty(t, origin)
	assert.Contains(t, err.Error(), "stored artifact origin")
	assert.Contains(t, err.Error(), "invalid artifact origin")
}

// TestEnsureOriginBootstrapsPreExistingLocalSessions verifies the deviation-2
// ordering: sessions written before an artifact origin exists are invisible
// to the origin-gated queue triggers and enqueue hooks, so EnsureOrigin must
// bootstrap the queue immediately after it persists the origin key.
func TestEnsureOriginBootstrapsPreExistingLocalSessions(t *testing.T) {
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	seedSession(t, database, "sess-2", "alpha")

	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Empty(t, pending, "no origin yet: queue triggers stay gated")

	origin, err := EnsureOrigin(database)
	require.NoError(t, err)
	require.NotEmpty(t, origin)

	pending, err = database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	assert.ElementsMatch(t, []string{"sess-1", "sess-2"}, []string{
		pending[0].SessionID, pending[1].SessionID,
	})
}

func TestEnsureOriginRollsBackWhenBootstrapFails(t *testing.T) {
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")

	injected := errors.New("bootstrap exploded")
	bootstrapExportQueue = func(*db.DB) error { return injected }
	t.Cleanup(func() { bootstrapExportQueue = (*db.DB).BootstrapArtifactExportQueue })

	_, err := EnsureOrigin(database)
	require.ErrorIs(t, err, injected)
	stored, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Empty(t, stored, "failed bootstrap must roll the origin back")

	bootstrapExportQueue = (*db.DB).BootstrapArtifactExportQueue
	origin, err := EnsureOrigin(database)
	require.NoError(t, err)
	require.NotEmpty(t, origin, "retry after rollback must re-run creation")
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1, "retry must re-run the bootstrap")
	assert.Equal(t, "sess-1", pending[0].SessionID)
}

func TestAdoptOriginRestoresPreviousOriginWhenBootstrapFails(t *testing.T) {
	database := testDB(t)
	require.NoError(t, AdoptOrigin(database, "before-a1b2c3"))

	injected := errors.New("bootstrap exploded")
	bootstrapExportQueue = func(*db.DB) error { return injected }
	t.Cleanup(func() { bootstrapExportQueue = (*db.DB).BootstrapArtifactExportQueue })

	err := AdoptOrigin(database, "after-d4e5f6")
	require.ErrorIs(t, err, injected)
	stored, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, "before-a1b2c3", stored,
		"failed adoption must restore the previous origin")
}

// TestAdoptOriginBootstrapsPreExistingLocalSessions mirrors the EnsureOrigin
// case for the AdoptOrigin path (used when a config-declared origin is
// applied to a database that predates it).
func TestAdoptOriginBootstrapsPreExistingLocalSessions(t *testing.T) {
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")

	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Empty(t, pending, "no origin yet: queue triggers stay gated")

	require.NoError(t, AdoptOrigin(database, "desk-a1b2c3"))

	pending, err = database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "sess-1", pending[0].SessionID)
}

func testDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })
	return database
}

func seedSession(t *testing.T, database *db.DB, id, project string, opts ...func(*db.Session)) {
	t.Helper()
	sess := db.Session{
		ID:               id,
		Project:          project,
		Machine:          "local",
		Agent:            "claude",
		MessageCount:     2,
		UserMessageCount: 1,
		FirstMessage:     new("hello"),
		StartedAt:        new("2026-06-14T01:02:03Z"),
		EndedAt:          new("2026-06-14T01:03:03Z"),
		SessionName:      new("Test Session"),
		CreatedAt:        "2026-06-14T01:02:03Z",
	}
	for _, opt := range opts {
		opt(&sess)
	}
	require.NoError(t, database.UpsertSession(sess))
	require.NoError(t, database.ReplaceSessionMessages(id, []db.Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: id, Ordinal: 1, Role: "assistant", Content: "world", ContentLength: 5},
	}))
}
