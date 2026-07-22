package artifact

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// syncInstance models one machine in a two-instance artifact-sync harness: its
// own database, data dir, origin, and metadata recorder, all driven through the
// public folder-sync API against a shared target folder.
type syncInstance struct {
	t       *testing.T
	db      *db.DB
	dataDir string
	origin  string
	now     time.Time
	repo    *Repository
	rec     *MetadataRecorder
}

func newSyncInstance(t *testing.T, origin string) *syncInstance {
	t.Helper()
	in := &syncInstance{
		t:       t,
		db:      testDB(t),
		dataDir: t.TempDir(),
		origin:  origin,
		now:     fixedHLCTime(),
	}
	var err error
	in.repo, err = OpenRepository(t.Context(), in.dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, in.repo.Close()) })
	in.rec = NewMetadataRecorder(in.db, MetadataRecorderOptions{
		Store:  in.repo.Store(),
		Origin: origin,
		Now:    func() time.Time { return in.now },
	})
	return in
}

// at sets the wall clock used for the instance's next local metadata edits.
func (in *syncInstance) at(ts time.Time) *syncInstance {
	in.now = ts
	return in
}

// sync exports local sessions to the shared target, exchanges the union both
// ways, and imports foreign origins, mirroring `agentsview sync <folder>`.
func (in *syncInstance) sync(target string) SyncResult {
	in.t.Helper()
	res, err := SyncWithStore(context.Background(), in.db, in.repo.Store(), SyncOptions{
		DataDir: in.dataDir,
		Target:  target,
		Origin:  in.origin,
		Now:     func() time.Time { return in.now },
	})
	require.NoError(in.t, err)
	return res
}

// syncInit mirrors `agentsview sync --init <folder>` for baseline metadata
// initialization.
func (in *syncInstance) syncInit(target string) SyncResult {
	in.t.Helper()
	res, err := SyncWithStore(context.Background(), in.db, in.repo.Store(), SyncOptions{
		DataDir:          in.dataDir,
		Target:           target,
		Origin:           in.origin,
		Now:              func() time.Time { return in.now },
		BaselineMetadata: true,
	})
	require.NoError(in.t, err)
	return res
}

// rename mirrors the rename handler: mutate the local row, then append the
// metadata event (which also records the local LWW register entry).
func (in *syncInstance) rename(localID, name string) {
	in.t.Helper()
	require.NoError(in.t, in.db.RenameSession(localID, &name))
	value, err := json.Marshal(struct {
		DisplayName string `json:"display_name"`
	}{DisplayName: name})
	require.NoError(in.t, err)
	_, err = in.rec.Append(context.Background(), MetadataEventInput{
		SessionID: localID,
		Op:        MetadataOpRename,
		Value:     value,
	})
	require.NoError(in.t, err)
}

// star mirrors the star handler: star the local row, then append the event.
func (in *syncInstance) star(localID string) {
	in.t.Helper()
	_, err := in.db.StarSession(localID)
	require.NoError(in.t, err)
	_, err = in.rec.Append(context.Background(), MetadataEventInput{
		SessionID: localID,
		Op:        MetadataOpStar,
	})
	require.NoError(in.t, err)
}

// purge mirrors the permanent-delete handler: soft delete, permanently delete
// from trash, then append the purge event.
func (in *syncInstance) purge(localID string) {
	in.t.Helper()
	require.NoError(in.t, in.db.SoftDeleteSession(localID))
	_, err := in.db.DeleteSessionIfTrashed(localID)
	require.NoError(in.t, err)
	_, err = in.rec.Append(context.Background(), MetadataEventInput{
		SessionID: localID,
		Op:        MetadataOpPurge,
	})
	require.NoError(in.t, err)
}

func (in *syncInstance) displayName(t *testing.T, id string) *string {
	t.Helper()
	got, err := in.db.GetSession(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, got)
	return got.DisplayName
}

func (in *syncInstance) requireSession(t *testing.T, id string) *db.Session {
	t.Helper()
	got, err := in.db.GetSession(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, got, "session %s should exist", id)
	return got
}

func (in *syncInstance) isStarred(t *testing.T, id string) bool {
	t.Helper()
	ids, err := in.db.ListStarredSessionIDs(context.Background())
	require.NoError(t, err)
	return slices.Contains(ids, id)
}

func TestTwoInstanceSessionAndRenamePropagate(t *testing.T) {
	target := t.TempDir()
	a := newSyncInstance(t, "laptop-a1b2c3")
	b := newSyncInstance(t, "desktop-d4e5f6")
	gid := a.origin + "~sess-1"

	seedSession(t, a.db, "sess-1", "alpha")
	a.at(fixedHLCTime()).rename("sess-1", "Renamed on A")

	a.sync(target)
	res := b.sync(target)
	assert.Equal(t, 1, res.ImportedSessions)
	assert.GreaterOrEqual(t, res.ImportedMessages, 1)
	assert.Equal(t, 1, res.ImportedMetadata)

	got, err := b.db.GetSession(context.Background(), gid)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, "Renamed on A", *got.DisplayName)
	assert.Equal(t, a.origin, got.Machine)

	// Re-syncing is idempotent: no new rows, no new conflicts.
	res = b.sync(target)
	assert.Zero(t, res.ImportedSessions)
	assert.Zero(t, res.ImportedMetadata)
}

func TestTwoInstanceConcurrentRenameConverges(t *testing.T) {
	target := t.TempDir()
	a := newSyncInstance(t, "laptop-a1b2c3")
	b := newSyncInstance(t, "desktop-d4e5f6")
	gid := a.origin + "~sess-1"
	bLocalID := gid // the session is foreign on B, keyed by its global id

	// Share the session from A to B first so both can edit it.
	seedSession(t, a.db, "sess-1", "alpha")
	a.sync(target)
	b.sync(target)
	b.requireSession(t, bLocalID)

	// Both rename the same session concurrently. A uses the later HLC (within the
	// clock drift bound so the receiver can observe it), so A wins deterministically
	// on both machines.
	b.at(fixedHLCTime()).rename(bLocalID, "Renamed on B")
	a.at(fixedHLCTime().Add(time.Minute)).rename("sess-1", "Renamed on A")

	// Exchange until quiescent: A publishes its edit, B pulls it (A wins on B),
	// then A pulls B's losing edit and records the conflict.
	a.sync(target)
	b.sync(target)
	a.sync(target)

	require.NotNil(t, a.displayName(t, "sess-1"))
	require.NotNil(t, b.displayName(t, bLocalID))
	assert.Equal(t, "Renamed on A", *a.displayName(t, "sess-1"))
	assert.Equal(t, "Renamed on A", *b.displayName(t, bLocalID))

	// Both machines independently record exactly one losing edit for the field.
	assertMetadataConflictCount(t, a.db, gid, "display_name", 1)
	assertMetadataConflictCount(t, b.db, gid, "display_name", 1)
}

func TestSyncInitDoesNotLetBaselineOutrankExistingPeerMetadata(t *testing.T) {
	target := t.TempDir()
	a := newSyncInstance(t, "laptop-a1b2c3")
	b := newSyncInstance(t, "desktop-d4e5f6")
	gid := a.origin + "~sess-1"

	seedSession(t, a.db, "sess-1", "alpha")
	a.sync(target)
	b.sync(target)
	b.requireSession(t, gid)

	peerName := "Peer newer title"
	b.at(fixedHLCTime().Add(time.Minute)).rename(gid, peerName)
	b.sync(target)

	staleLocalName := "Stale local title"
	require.NoError(t, a.db.RenameSession("sess-1", &staleLocalName))

	res := a.at(fixedHLCTime().Add(2 * time.Minute)).syncInit(target)
	assert.Equal(t, 1, res.ImportedMetadata)
	require.NotNil(t, a.displayName(t, "sess-1"))
	assert.Equal(t, peerName, *a.displayName(t, "sess-1"))

	b.sync(target)
	require.NotNil(t, b.displayName(t, gid))
	assert.Equal(t, peerName, *b.displayName(t, gid))
	assertMetadataConflictCount(t, a.db, gid, "display_name", 0)
	assertMetadataConflictCount(t, b.db, gid, "display_name", 0)
}

func TestSyncInitDoesNotBaselineRowsCreatedByPreBaselineImport(t *testing.T) {
	target := t.TempDir()
	a := newSyncInstance(t, "laptop-a1b2c3")
	b := newSyncInstance(t, "desktop-d4e5f6")
	gid := b.origin + "~sess-1"

	seedSession(t, b.db, "sess-1", "alpha")
	b.sync(target)

	require.NoError(t, a.db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			CREATE TRIGGER test_imported_display_name
			AFTER INSERT ON sessions
			WHEN NEW.id = 'desktop-d4e5f6~sess-1'
			BEGIN
				UPDATE sessions
				SET display_name = 'Imported stale title'
				WHERE id = NEW.id;
			END`)
		return err
	}))

	res := a.syncInit(target)
	assert.Equal(t, 1, res.ImportedSessions)
	assert.Equal(t, 0, res.ImportedMetadata)
	require.NotNil(t, a.displayName(t, gid))
	assert.Equal(t, "Imported stale title", *a.displayName(t, gid))

	_, ok, err := a.db.MetadataReplayStateOp(context.Background(), gid, "display_name")
	require.NoError(t, err)
	assert.False(t, ok, "import-created curation must not become local baseline metadata")
	assert.Empty(t, globArtifacts(t, target, a.origin, "meta", "*"+metadataEventExtension))
}

func TestSyncReappliesMetadataReplayStateAfterManifestRefresh(t *testing.T) {
	target := t.TempDir()
	a := newSyncInstance(t, "laptop-a1b2c3")
	b := newSyncInstance(t, "desktop-d4e5f6")
	gid := b.origin + "~sess-1"

	sourceName := "Source title"
	seedSession(t, b.db, "sess-1", "alpha", func(s *db.Session) {
		s.SessionName = &sourceName
	})
	b.sync(target)
	a.sync(target)
	a.requireSession(t, gid)

	localName := "Local winning title"
	a.at(fixedHLCTime().Add(time.Minute)).rename(gid, localName)
	require.NotNil(t, a.displayName(t, gid))
	assert.Equal(t, localName, *a.displayName(t, gid))

	require.NoError(t, a.db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sessions SET display_name = NULL WHERE id = ?`, gid)
		return err
	}))
	refreshedSourceName := "Refreshed source title"
	seedSession(t, b.db, "sess-1", "alpha", func(s *db.Session) {
		s.SessionName = &refreshedSourceName
	})
	b.sync(target)

	a.sync(target)
	require.NotNil(t, a.displayName(t, gid))
	assert.Equal(t, localName, *a.displayName(t, gid))
}

func TestTwoInstanceStarAndPurgePropagate(t *testing.T) {
	target := t.TempDir()
	a := newSyncInstance(t, "laptop-a1b2c3")
	b := newSyncInstance(t, "desktop-d4e5f6")
	gid := a.origin + "~sess-1"

	seedSession(t, a.db, "sess-1", "alpha")
	a.sync(target)
	b.sync(target)
	b.requireSession(t, gid)

	// B stars the shared session; the star converges back to A.
	b.at(fixedHLCTime()).star(gid)
	b.sync(target)
	a.sync(target)
	assert.True(t, a.isStarred(t, "sess-1"))
	assert.True(t, b.isStarred(t, gid))

	// A purges the session; the purge tombstone propagates to B and blocks
	// re-import of the now-superseded manifest. The HLC stays within the drift
	// bound so B can observe it on import.
	a.at(fixedHLCTime().Add(time.Minute)).purge("sess-1")
	a.sync(target)
	b.sync(target)

	gotA, err := a.db.GetSession(context.Background(), "sess-1")
	require.NoError(t, err)
	assert.Nil(t, gotA)
	gotB, err := b.db.GetSession(context.Background(), gid)
	require.NoError(t, err)
	assert.Nil(t, gotB)

	// A later sync must not resurrect the purged session on B.
	b.sync(target)
	gotB, err = b.db.GetSession(context.Background(), gid)
	require.NoError(t, err)
	assert.Nil(t, gotB)
}
