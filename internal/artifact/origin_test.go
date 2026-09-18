package artifact

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestEnsureOriginPersists(t *testing.T) {
	require := require.New(t)

	t.Parallel()

	database := testDB(t)

	first, err := EnsureOrigin(database)
	require.NoError(err)
	require.NotEmpty(first)
	require.NotEqual("local", first)

	second, err := EnsureOrigin(database)
	require.NoError(err)
	assert.Equal(t, first, second)
}

func TestAdoptOriginPersistsConfigOrigin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	database := testDB(t)

	require.NoError(AdoptOrigin(database, "desk-a1b2c3"))

	stored, err := StoredOrigin(database)
	require.NoError(err)
	assert.Equal("desk-a1b2c3", stored)

	// EnsureOrigin and its callers now agree with the adopted origin instead
	// of generating a divergent DB-only value.
	ensured, err := EnsureOrigin(database)
	require.NoError(err)
	assert.Equal("desk-a1b2c3", ensured)
}

func TestAdoptOriginIsIdempotent(t *testing.T) {
	require := require.New(t)

	t.Parallel()

	database := testDB(t)

	require.NoError(AdoptOrigin(database, "desk-a1b2c3"))
	require.NoError(AdoptOrigin(database, "desk-a1b2c3"))

	stored, err := StoredOrigin(database)
	require.NoError(err)
	assert.Equal(t, "desk-a1b2c3", stored)
}

func TestAdoptOriginOverwritesDivergentDBOrigin(t *testing.T) {
	require := require.New(t)

	t.Parallel()

	database := testDB(t)

	// Simulate the pre-fix state: the recorder generated a DB-only origin
	// before the authoritative config origin existed.
	stale, err := EnsureOrigin(database)
	require.NoError(err)
	require.NotEqual("desk-a1b2c3", stale)

	require.NoError(AdoptOrigin(database, "desk-a1b2c3"))

	stored, err := StoredOrigin(database)
	require.NoError(err)
	assert.Equal(t, "desk-a1b2c3", stored)
}

func TestAdoptOriginRepairsInvalidPersistedOrigin(t *testing.T) {
	require := require.New(t)

	t.Parallel()

	database := testDB(t)
	require.NoError(database.SetSyncState(originStateKey, "../outside"))

	require.NoError(AdoptOrigin(database, "desk-a1b2c3"))

	stored, err := StoredOrigin(database)
	require.NoError(err)
	assert.Equal(t, "desk-a1b2c3", stored)
}

func TestAdoptOriginRejectsInvalidOrigin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	database := testDB(t)

	err := AdoptOrigin(database, "../outside")
	require.Error(err)
	assert.Contains(err.Error(), "adopting artifact origin")

	stored, err := StoredOrigin(database)
	require.NoError(err)
	assert.Empty(stored)
}

func TestEnsureOriginRejectsInvalidPersistedOrigin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	database := testDB(t)
	require.NoError(database.SetSyncState(originStateKey, "../outside"))

	origin, err := EnsureOrigin(database)
	require.Error(err)
	assert.Empty(origin)
	assert.Contains(err.Error(), "stored artifact origin")
	assert.Contains(err.Error(), "invalid artifact origin")
}

// TestEnsureOriginBootstrapsPreExistingLocalSessions verifies the deviation-2
// ordering: sessions written before an artifact origin exists are invisible
// to the origin-gated queue triggers and enqueue hooks, so EnsureOrigin must
// bootstrap the queue immediately after it persists the origin key.
func TestEnsureOriginBootstrapsPreExistingLocalSessions(t *testing.T) {
	require := require.New(t)

	t.Parallel()

	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	seedSession(t, database, "sess-2", "alpha")

	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Empty(pending, "no origin yet: queue triggers stay gated")

	origin, err := EnsureOrigin(database)
	require.NoError(err)
	require.NotEmpty(origin)

	pending, err = database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Len(pending, 2)
	assert.ElementsMatch(t, []string{"sess-1", "sess-2"}, []string{
		pending[0].SessionID, pending[1].SessionID,
	})
}

// TestAdoptOriginRequeuesAllExportsOnDivergentAdoption covers the divergent
// adoption path: when a new origin replaces an established one whose sessions
// are already acknowledged, INSERT OR IGNORE bootstrap would leave the ledger
// empty, so every owned session must be force-requeued with a bumped
// generation.
func TestAdoptOriginRequeuesAllExportsOnDivergentAdoption(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	database := testDB(t)
	require.NoError(AdoptOrigin(database, "origin-a1b2c3"))
	seedSession(t, database, "sess-1", "alpha")
	seedSession(t, database, "sess-2", "alpha")

	ctx := t.Context()
	pending, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(pending, 2)
	genBefore := map[string]int64{}
	for _, item := range pending {
		genBefore[item.SessionID] = item.Generation
	}

	// Simulate the prior origin having fully published every session.
	require.NoError(database.AcknowledgeArtifactExports(ctx, pending))
	drained, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Empty(drained)

	require.NoError(AdoptOrigin(database, "origin-d4e5f6"))
	pending, err = database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(pending, 2, "divergent adoption re-verifies every owned session")
	assert.ElementsMatch([]string{"sess-1", "sess-2"}, []string{
		pending[0].SessionID, pending[1].SessionID,
	})
	for _, item := range pending {
		assert.Greater(item.Generation, genBefore[item.SessionID],
			"divergent adoption must bump the generation of every requeued session")
	}
}

func TestAdoptOriginRemovesPublicationsThatBecameDeletedWhileOriginWasInactive(t *testing.T) {
	require := require.New(t)

	t.Parallel()

	database := testDB(t)
	store, err := newProtocolTestStore(t.TempDir())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })

	const (
		originA = "origin-a1b2c3"
		originB = "origin-d4e5f6"
	)
	require.NoError(AdoptOrigin(database, originA))
	seedSession(t, database, "sess-1", "alpha")
	_, err = ExportToStore(t.Context(), database, store, ExportOptions{Origin: originA})
	require.NoError(err)
	require.Contains(latestStoreCheckpointForTest(t, store, originA).Sessions,
		originA+"~sess-1")

	require.NoError(AdoptOrigin(database, originB))
	require.NoError(database.SoftDeleteSession("sess-1"))
	_, err = ExportToStore(t.Context(), database, store, ExportOptions{Origin: originB})
	require.NoError(err)
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Empty(pending)

	require.NoError(AdoptOrigin(database, originA))
	_, err = ExportToStore(t.Context(), database, store, ExportOptions{Origin: originA})
	require.NoError(err)
	assert.NotContains(t, latestStoreCheckpointForTest(t, store, originA).Sessions,
		originA+"~sess-1")
}

func TestOriginRejectionLifecycleRemovesRetriesAndRequeues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	database := testDB(t)
	store, err := newProtocolTestStore(t.TempDir())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })

	const (
		originA    = "origin-a1b2c3"
		originB    = "origin-d4e5f6"
		sessionID  = "sess-1"
		sessionGID = originA + "~" + sessionID
	)
	require.NoError(AdoptOrigin(database, originA))
	seedSession(t, database, sessionID, "alpha")
	_, err = ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: originA,
	})
	require.NoError(err)
	assert.Contains(latestStoreCheckpointForTest(t, store, originA).Sessions,
		sessionGID)

	require.NoError(database.ReplaceSessionMessages(sessionID, []db.Message{
		{SessionID: sessionID, Ordinal: 0, Role: "user", Content: "one"},
		{SessionID: sessionID, Ordinal: 1, Role: "assistant", Content: "two"},
		{SessionID: sessionID, Ordinal: 2, Role: "user", Content: "three"},
	}))
	limits := productionArtifactLimits()
	limits.sessionMessages = 2
	result, err := exportToStoreWithLimits(
		t.Context(), database, store, ExportOptions{Origin: originA}, limits,
	)
	require.NoError(err)
	assert.Equal(1, result.RejectedSessions)
	assert.NotContains(latestStoreCheckpointForTest(t, store, originA).Sessions,
		sessionGID)
	rejection, ok, err := database.GetArtifactExportRejection(t.Context(), sessionID)
	require.NoError(err)
	require.True(ok)
	assert.Contains(rejection.Error, "message count exceeds 2")

	require.NoError(database.ReplaceSessionMessages(sessionID, []db.Message{
		{SessionID: sessionID, Ordinal: 0, Role: "user", Content: "fixed"},
	}))
	_, ok, err = database.GetArtifactExportRejection(t.Context(), sessionID)
	require.NoError(err)
	assert.False(ok, "a new generation clears the prior rejection")
	result, err = exportToStoreWithLimits(
		t.Context(), database, store, ExportOptions{Origin: originA}, limits,
	)
	require.NoError(err)
	assert.Equal(1, result.ExportedSessions)
	assert.Contains(latestStoreCheckpointForTest(t, store, originA).Sessions,
		sessionGID)

	require.NoError(AdoptOrigin(database, originB))
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(sessionID, pending[0].SessionID)
	for _, item := range pending {
		_, rejected, rejectionErr := database.GetArtifactExportRejection(
			t.Context(), item.SessionID,
		)
		require.NoError(rejectionErr)
		assert.False(rejected,
			"origin adoption must not carry stale rejection diagnostics")
	}
}

// TestAdoptOriginBootstrapsPreExistingLocalSessions mirrors the EnsureOrigin
// case for the AdoptOrigin path (used when a config-declared origin is
// applied to a database that predates it).
func TestAdoptOriginBootstrapsPreExistingLocalSessions(t *testing.T) {
	require := require.New(t)

	t.Parallel()

	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")

	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Empty(pending, "no origin yet: queue triggers stay gated")

	require.NoError(AdoptOrigin(database, "desk-a1b2c3"))

	pending, err = database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Len(pending, 1)
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
