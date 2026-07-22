package artifact

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func importFromTestStore(
	ctx context.Context, database *db.DB, store ArtifactStore, localOrigin string,
) (int, int, error) {
	result, err := ImportDetailedFromStore(ctx, database, store, localOrigin)
	return result.Sessions, result.Messages, err
}

type failingHLCStateStore struct {
	err error
}

func (s failingHLCStateStore) GetSyncState(string) (string, error) { return "", nil }
func (s failingHLCStateStore) SetSyncState(string, string) error   { return s.err }

func createStoreMetadataEvent(
	t *testing.T, store ArtifactStore, origin string, event metadataEvent,
) Ref {
	t.Helper()
	data, err := canonicalJSON(event)
	require.NoError(t, err)
	hash := hashHex(data)
	stamp, err := ParseHLCTimestamp(event.HLC)
	require.NoError(t, err)
	ref, err := NewRef(origin, KindMeta, stamp.OrderingKey(hash)+metadataEventExtension)
	require.NoError(t, err)
	identity, err := NewIdentity(hash, int64(len(data)))
	require.NoError(t, err)
	_, err = store.Create(t.Context(), ref, identity,
		canonicalArtifactMediaType(KindMeta), bytes.NewReader(data))
	require.NoError(t, err)
	return ref
}

func TestReplayMetadataFromStorePropagatesHLCPersistenceFailure(t *testing.T) {
	origin := "peer-a1b2c3"
	store, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	event := replayRenameEvent(
		t, origin, origin+"~session-1", replayTestHLC(0, 0), "renamed",
	)
	createStoreMetadataEvent(t, store, origin, event)
	persistErr := errors.New("injected HLC persistence failure")
	clock := NewHLCClock(failingHLCStateStore{err: persistErr}, HLCClockOptions{
		Now: fixedHLCTime,
	})

	_, err = replayMetadataFromStore(
		t.Context(), testDB(t), clock, store, origin, "local-d4e5f6",
	)
	require.ErrorIs(t, err, persistErr)
}

func TestReplayMetadataFromStoreQuarantinesInvalidCurrentOpBeforeDrift(t *testing.T) {
	origin := "peer-a1b2c3"
	store, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	farFuture := HLCTimestamp{WallTime: fixedHLCTime().Add(24 * time.Hour)}
	ref := createStoreMetadataEvent(t, store, origin, metadataEvent{
		Version: formatVersion, HLC: farFuture.String(), Origin: origin,
		SessionGID: origin + "~session-1", Op: "unsupported-current-op",
	})
	clock := NewHLCClock(testDB(t), HLCClockOptions{
		Now: fixedHLCTime, MaxDrift: time.Minute,
	})

	changed, err := replayMetadataFromStore(
		t.Context(), testDB(t), clock, store, origin, "local-d4e5f6",
	)
	require.NoError(t, err)
	assert.Zero(t, changed)
	_, err = store.Stat(t.Context(), ref)
	assert.ErrorIs(t, err, ErrArtifactNotFound,
		"unsupported current operations are quarantined before HLC drift deferral")
}

func TestReplayMetadataFromStoreDefersNoncanonicalFutureEventWithoutQuarantine(t *testing.T) {
	origin := "peer-a1b2c3"
	now := fixedHLCTime()
	remote := HLCTimestamp{WallTime: now.Add(time.Minute)}
	// A newer binary may have written a target schema that this binary's ingress
	// does not understand. Replay must preserve that immutable event for upgrade
	// even though the current metadataEvent type cannot represent its payload.
	tests := []struct {
		name string
		body string
	}{
		{
			name: "HLC absent",
			body: `{"future_target":{"session":"peer-a1b2c3~session-1"},"v":%d}`,
		},
		{
			name: "HLC has future shape",
			body: `{"hlc":{"wall":"later-schema"},"future_target":true,"v":%d}`,
		},
		{
			name: "HLC string does not match filename",
			body: `{"hlc":"future-format-value","future_target":true,"v":%d}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := newProtocolTestStore(t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			data := fmt.Appendf(nil, tt.body, formatVersion+1)
			hash := hashHex(data)
			ref, err := NewRef(origin, KindMeta,
				remote.OrderingKey(hash)+metadataEventExtension)
			require.NoError(t, err)
			createContractArtifact(t, store, ref, data)
			database := testDB(t)
			clock := NewHLCClock(database, HLCClockOptions{
				Now: func() time.Time { return now },
			})

			changed, err := replayMetadataFromStore(
				t.Context(), database, clock, store, origin, "local-d4e5f6",
			)
			require.NoError(t, err)
			assert.Zero(t, changed)
			_, err = store.Stat(t.Context(), ref)
			require.NoError(t, err, "future metadata must remain available for a later upgrade")
			next, err := clock.Next()
			require.NoError(t, err)
			assert.Negative(t, next.Compare(remote),
				"a future clock shape must not be interpreted as today's filename HLC")
			assert.Equal(t, 0, metadataAppliedCount(t, database, origin))
		})
	}
}

func TestImportDetailedFromStoreLoadsAllContentBeforeCrossOriginMetadata(t *testing.T) {
	metadataOrigin := "alpha-a1b2c3"
	contentOrigin := "zulu-d4e5f6"
	store, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	source := testDB(t)
	seedSession(t, source, "session-1", "alpha")
	_, err = ExportToStore(t.Context(), source, store, ExportOptions{
		Origin: contentOrigin, Full: true,
	})
	require.NoError(t, err)
	createStoreMetadataEvent(t, store, metadataOrigin, replayRenameEvent(
		t, metadataOrigin, contentOrigin+"~session-1", replayTestHLC(0, 0), "cross-origin rename",
	))
	target := testDB(t)

	result, err := ImportDetailedFromStore(t.Context(), target, store, "local-c7d8e9")
	require.NoError(t, err)
	assert.Equal(t, 1, result.Sessions)
	assert.Equal(t, 1, result.Metadata)
	imported, err := target.GetSession(t.Context(), contentOrigin+"~session-1")
	require.NoError(t, err)
	require.NotNil(t, imported)
	require.NotNil(t, imported.DisplayName)
	assert.Equal(t, "cross-origin rename", *imported.DisplayName)
}

func TestImportReplaysMetadataRenameDeterministically(t *testing.T) {
	ctx := context.Background()
	origin := "laptop-a1b2c3"
	localOrigin := "desktop-d4e5f6"
	gid := origin + "~sess-1"
	stamp := replayTestHLC(0, 7)
	events := []metadataEvent{
		replayRenameEvent(t, origin, gid, stamp, "Alpha"),
		replayRenameEvent(t, origin, gid, stamp, "Beta"),
	}

	for _, tc := range []struct {
		name       string
		writeOrder []int
	}{
		{name: "forward write order", writeOrder: []int{0, 1}},
		{name: "reverse write order", writeOrder: []int{1, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestArtifactStore(t)
			exportDB := testDB(t)
			importDB := testDB(t)
			seedSession(t, exportDB, "sess-1", "alpha")
			_, err := ExportToStore(ctx, exportDB, store, ExportOptions{Origin: origin, Full: true})
			require.NoError(t, err)

			arts := make([]metadataArtifact, len(events))
			for _, idx := range tc.writeOrder {
				arts[idx] = writeMetadataArtifact(t, store, events[idx])
			}
			wantName := "Alpha"
			if arts[1].orderKey > arts[0].orderKey {
				wantName = "Beta"
			}

			imported, messages, err := importFromTestStore(ctx, importDB, store, localOrigin)
			require.NoError(t, err)
			assert.Equal(t, 1, imported)
			assert.Equal(t, 2, messages)

			got, err := importDB.GetSession(ctx, gid)
			require.NoError(t, err)
			require.NotNil(t, got)
			require.NotNil(t, got.DisplayName)
			assert.Equal(t, wantName, *got.DisplayName)
			assertMetadataConflictCount(t, importDB, gid, "display_name", 0)
			assert.Equal(t, 2, metadataAppliedCount(t, importDB, origin))
			provenance, err := importDB.MetadataArtifactProvenanceForSession(
				ctx, origin, gid, MetadataOpRename,
			)
			require.NoError(t, err)
			assert.Len(t, provenance, 2,
				"import records point-read provenance for each validated event")

			imported, messages, err = importFromTestStore(ctx, importDB, store, localOrigin)
			require.NoError(t, err)
			assert.Zero(t, imported)
			assert.Zero(t, messages)
			assertMetadataConflictCount(t, importDB, gid, "display_name", 0)
			assert.Equal(t, 2, metadataAppliedCount(t, importDB, origin))
		})
	}
}

func TestImportAppliesEarlierOriginMetadataToLaterOriginContentInOnePass(t *testing.T) {
	ctx := context.Background()
	metadataOrigin := "alpha-a1b2c3"
	contentOrigin := "zulu-d4e5f6"
	localOrigin := "desktop-c7d8e9"
	store := newTestArtifactStore(t)
	exportDB := testDB(t)
	importDB := testDB(t)
	seedSession(t, exportDB, "sess-1", "alpha")
	_, err := ExportToStore(ctx, exportDB, store, ExportOptions{Origin: contentOrigin, Full: true})
	require.NoError(t, err)

	gid := contentOrigin + "~sess-1"
	writeMetadataArtifact(t, store, replayRenameEvent(
		t, metadataOrigin, gid, replayTestHLC(0, 0), "Renamed before import",
	))
	writeMetadataArtifact(t, store, metadataEvent{
		Version:    formatVersion,
		HLC:        replayTestHLC(time.Nanosecond, 0),
		Origin:     metadataOrigin,
		SessionGID: gid,
		Op:         MetadataOpSoftDelete,
	})

	res, err := ImportDetailedFromStore(ctx, importDB, store, localOrigin)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Sessions)
	assert.Equal(t, 2, res.Messages)
	assert.Equal(t, 2, res.Metadata)

	var displayName, deletedAt sql.NullString
	err = importDB.Reader().QueryRowContext(ctx,
		`SELECT display_name, deleted_at FROM sessions WHERE id = ?`, gid,
	).Scan(&displayName, &deletedAt)
	require.NoError(t, err)
	require.True(t, displayName.Valid)
	assert.Equal(t, "Renamed before import", displayName.String)
	require.True(t, deletedAt.Valid)
	assert.Equal(t, "2026-06-14T01:02:03.000Z", deletedAt.String)
	assert.Equal(t, 2, metadataAppliedCount(t, importDB, metadataOrigin))
}

func TestImportSkipsUnknownMetadataOpAndContinues(t *testing.T) {
	ctx := context.Background()
	origin := "laptop-a1b2c3"
	localOrigin := "desktop-d4e5f6"
	store := newTestArtifactStore(t)
	gid := origin + "~sess-1"
	exportDB := testDB(t)
	importDB := testDB(t)
	seedSession(t, exportDB, "sess-1", "alpha")
	_, err := ExportToStore(ctx, exportDB, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)

	unknown := metadataEvent{
		Version:    formatVersion,
		HLC:        replayTestHLC(0, 0),
		Origin:     origin,
		SessionGID: gid,
		Op:         "future_tag",
		Value:      json.RawMessage(`{"tag":"later"}`),
	}
	writeMetadataArtifact(t, store, unknown)
	writeMetadataArtifact(t, store,
		replayRenameEvent(t, origin, gid, replayTestHLC(time.Nanosecond, 0), "Known winner"))

	_, _, err = importFromTestStore(ctx, importDB, store, localOrigin)
	require.NoError(t, err)

	got, err := importDB.GetSession(ctx, gid)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, "Known winner", *got.DisplayName)
	assert.Equal(t, 2, metadataAppliedCount(t, importDB, origin))
}

// A known op with a malformed payload (here: pin without its pin
// payload) is immutable and can never become applicable, so replay
// must quarantine it as applied instead of aborting the origin.
func TestImportQuarantinesMalformedKnownOpAndContinues(t *testing.T) {
	ctx := context.Background()
	origin := "laptop-a1b2c3"
	localOrigin := "desktop-d4e5f6"
	store := newTestArtifactStore(t)
	gid := origin + "~sess-1"
	exportDB := testDB(t)
	importDB := testDB(t)
	seedSession(t, exportDB, "sess-1", "alpha")
	_, err := ExportToStore(ctx, exportDB, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)

	malformed := metadataEvent{
		Version:    formatVersion,
		HLC:        replayTestHLC(0, 0),
		Origin:     origin,
		SessionGID: gid,
		Op:         MetadataOpPin,
	}
	writeMetadataArtifact(t, store, malformed)
	writeMetadataArtifact(t, store,
		replayRenameEvent(t, origin, gid, replayTestHLC(time.Nanosecond, 0), "Known winner"))

	_, _, err = importFromTestStore(ctx, importDB, store, localOrigin)
	require.NoError(t, err)

	got, err := importDB.GetSession(ctx, gid)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, "Known winner", *got.DisplayName)
	// Both events are marked applied so replay never retries the
	// malformed one.
	assert.Equal(t, 2, metadataAppliedCount(t, importDB, origin))

	// A later sync neither errors nor reprocesses the malformed event.
	_, _, err = importFromTestStore(ctx, importDB, store, localOrigin)
	require.NoError(t, err)
	assert.Equal(t, 2, metadataAppliedCount(t, importDB, origin))
}

func TestImportSkipsUnparseableHLCMetadataEvent(t *testing.T) {
	ctx := context.Background()
	origin := "laptop-a1b2c3"
	localOrigin := "desktop-d4e5f6"
	store := newTestArtifactStore(t)
	gid := origin + "~sess-1"
	exportDB := testDB(t)
	importDB := testDB(t)
	seedSession(t, exportDB, "sess-1", "alpha")
	_, err := ExportToStore(ctx, exportDB, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)

	// A forged event whose HLC does not parse but whose filename hash is
	// valid. Its order key sorts lexicographically above every real
	// timestamp, so if accepted it would win LWW permanently (#1034).
	forgedHLC := "9999-99-99T999999.000000000Z-00000000000000000000"
	forged := replayRenameEvent(t, origin, gid, forgedHLC, "PWNED")
	data, err := canonicalJSON(forged)
	require.NoError(t, err)
	forgedRef, err := NewRef(
		origin, KindMeta, forgedHLC+"-"+hashHex(data)+metadataEventExtension,
	)
	require.NoError(t, err)
	createContractArtifact(t, store, forgedRef, data)
	writeMetadataArtifact(t, store,
		replayRenameEvent(t, origin, gid, replayTestHLC(0, 0), "Legit"))

	imported, messages, err := importFromTestStore(ctx, importDB, store, localOrigin)
	require.NoError(t, err)
	assert.Equal(t, 1, imported)
	assert.Equal(t, 2, messages)

	got, err := importDB.GetSession(ctx, gid)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, "Legit", *got.DisplayName)
	// Both events are marked applied so replay never retries the forged one.
	assert.Equal(t, 2, metadataAppliedCount(t, importDB, origin))

	// Repeat imports stay clean and never resurrect the forged value.
	_, _, err = importFromTestStore(ctx, importDB, store, localOrigin)
	require.NoError(t, err)
	got, err = importDB.GetSession(ctx, gid)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, "Legit", *got.DisplayName)
	assert.Equal(t, 2, metadataAppliedCount(t, importDB, origin))
}

func TestImportSoftDeleteUsesEventHLCForDeletedAt(t *testing.T) {
	ctx := context.Background()
	origin := "laptop-a1b2c3"
	localOrigin := "desktop-d4e5f6"
	store := newTestArtifactStore(t)
	gid := origin + "~sess-1"
	exportDB := testDB(t)
	importDB := testDB(t)
	seedSession(t, exportDB, "sess-1", "alpha")
	_, err := ExportToStore(ctx, exportDB, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)

	writeMetadataArtifact(t, store, metadataEvent{
		Version:    formatVersion,
		HLC:        replayTestHLC(0, 0),
		Origin:     origin,
		SessionGID: gid,
		Op:         MetadataOpSoftDelete,
	})

	_, _, err = importFromTestStore(ctx, importDB, store, localOrigin)
	require.NoError(t, err)

	var deletedAt sql.NullString
	err = importDB.Reader().QueryRowContext(ctx,
		`SELECT deleted_at FROM sessions WHERE id = ?`, gid).Scan(&deletedAt)
	require.NoError(t, err)
	require.True(t, deletedAt.Valid)
	// Trash retention must be anchored to the author-side event time, not the
	// moment each machine happened to import the event, or retention windows
	// drift across the fleet.
	assert.Equal(t, "2026-06-14T01:02:03.000Z", deletedAt.String)
}

func TestWriteArtifactRejectsUnparseableHLCMetadataEvent(t *testing.T) {
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	gid := origin + "~sess-1"
	forgedHLC := "9999-99-99T999999.000000000Z-00000000000000000000"
	forged := replayRenameEvent(t, origin, gid, forgedHLC, "PWNED")
	data, err := canonicalJSON(forged)
	require.NoError(t, err)
	name := forgedHLC + "-" + hashHex(data) + metadataEventExtension

	_, err = createCompressedTestArtifact(t, store, origin, KindMeta, name, data)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
}

func TestImportDefersFutureVersionMetadataEventAndContinues(t *testing.T) {
	ctx := context.Background()
	origin := "laptop-a1b2c3"
	localOrigin := "desktop-d4e5f6"
	store := newTestArtifactStore(t)
	gid := origin + "~sess-1"
	exportDB := testDB(t)
	importDB := testDB(t)
	seedSession(t, exportDB, "sess-1", "alpha")
	_, err := ExportToStore(ctx, exportDB, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)

	future := metadataEvent{
		Version:    formatVersion + 1,
		HLC:        replayTestHLC(0, 0),
		Origin:     origin,
		SessionGID: gid,
		Op:         "future_tag",
		Value:      json.RawMessage(`{"tag":"later"}`),
	}
	writeMetadataArtifact(t, store, future)
	writeMetadataArtifact(t, store,
		replayRenameEvent(t, origin, gid, replayTestHLC(time.Nanosecond, 0), "Known winner"))

	_, _, err = importFromTestStore(ctx, importDB, store, localOrigin)
	require.NoError(t, err)

	got, err := importDB.GetSession(ctx, gid)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, "Known winner", *got.DisplayName)
	assert.Equal(t, 1, metadataAppliedCount(t, importDB, origin))
}

func TestImportObservesFutureVersionMetadataHLCBeforeDeferring(t *testing.T) {
	ctx := context.Background()
	origin := "laptop-a1b2c3"
	localOrigin := "desktop-d4e5f6"
	store := newTestArtifactStore(t)
	importDB := testDB(t)
	now := fixedHLCTime()
	remote := HLCTimestamp{WallTime: now.Add(time.Minute)}
	event := metadataEvent{
		Version:    formatVersion + 1,
		HLC:        remote.String(),
		Origin:     origin,
		SessionGID: origin + "~sess-1",
		Op:         "future_tag",
		Value:      json.RawMessage(`{"tag":"later"}`),
	}
	writeMetadataArtifact(t, store, event)

	clock := NewHLCClock(importDB, HLCClockOptions{
		Now: func() time.Time { return now },
	})
	_, err := importDetailedFromStoreWithClock(ctx, importDB, store, localOrigin, clock)
	require.NoError(t, err)

	next, err := clock.Next()
	require.NoError(t, err)
	assert.Positive(t, next.Compare(remote),
		"a local edit after deferring a future-version event must sort after that peer event")
	assert.Equal(t, 0, metadataAppliedCount(t, importDB, origin))
}

func TestImportAlreadyAppliedMetadataDoesNotAdvanceResetHLC(t *testing.T) {
	ctx := context.Background()
	origin := "laptop-a1b2c3"
	localOrigin := "desktop-d4e5f6"
	store := newTestArtifactStore(t)
	exportDB := testDB(t)
	importDB := testDB(t)
	seedSession(t, exportDB, "sess-1", "alpha")
	_, err := ExportToStore(ctx, exportDB, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)

	now := fixedHLCTime()
	remote := HLCTimestamp{WallTime: now.Add(time.Minute)}
	writeMetadataArtifact(t, store,
		replayRenameEvent(t, origin, origin+"~sess-1", remote.String(), "Renamed"))
	clock := NewHLCClock(importDB, HLCClockOptions{Now: func() time.Time { return now }})
	_, err = importDetailedFromStoreWithClock(ctx, importDB, store, localOrigin, clock)
	require.NoError(t, err)
	assert.Equal(t, 1, metadataAppliedCount(t, importDB, origin))

	older := HLCTimestamp{WallTime: now.Add(-time.Minute)}.String()
	require.NoError(t, importDB.SetSyncState(metadataHLCStateKey, older))
	clock = NewHLCClock(importDB, HLCClockOptions{Now: func() time.Time { return now }})
	_, err = importDetailedFromStoreWithClock(ctx, importDB, store, localOrigin, clock)
	require.NoError(t, err)

	persisted, err := importDB.GetSyncState(metadataHLCStateKey)
	require.NoError(t, err)
	assert.Equal(t, older, persisted,
		"an already-applied event must be skipped before its HLC is observed")
}

func TestImportDoesNotAdvanceMetadataWatermarkWhenTargetMissing(t *testing.T) {
	ctx := context.Background()
	origin := "laptop-a1b2c3"
	localOrigin := "desktop-d4e5f6"
	store := newTestArtifactStore(t)
	importDB := testDB(t)
	writeMetadataArtifact(t, store,
		replayRenameEvent(t, origin, localOrigin+"~sess-1", replayTestHLC(0, 0), "Remote rename"))

	imported, messages, err := importFromTestStore(ctx, importDB, store, localOrigin)
	require.NoError(t, err)
	assert.Zero(t, imported)
	assert.Zero(t, messages)
	assert.Equal(t, 0, metadataAppliedCount(t, importDB, origin))

	seedSession(t, importDB, "sess-1", "alpha")
	imported, messages, err = importFromTestStore(ctx, importDB, store, localOrigin)
	require.NoError(t, err)
	assert.Zero(t, imported)
	assert.Zero(t, messages)

	got, err := importDB.GetSession(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, "Remote rename", *got.DisplayName)
	assert.Equal(t, 1, metadataAppliedCount(t, importDB, origin))
}

func TestImportReplaysMetadataPinAndUnpin(t *testing.T) {
	ctx := context.Background()
	origin := "laptop-a1b2c3"
	localOrigin := "desktop-d4e5f6"
	store := newTestArtifactStore(t)
	gid := origin + "~sess-1"
	exportDB := testDB(t)
	importDB := testDB(t)
	seedSession(t, exportDB, "sess-1", "alpha")
	require.NoError(t, exportDB.ReplaceSessionMessages("sess-1", []db.Message{
		{
			SessionID:     "sess-1",
			Ordinal:       0,
			Role:          "user",
			Content:       "hello",
			ContentLength: 5,
			SourceUUID:    "uuid-question",
		},
		{
			SessionID:     "sess-1",
			Ordinal:       1,
			Role:          "assistant",
			Content:       "world",
			ContentLength: 5,
			SourceUUID:    "uuid-answer",
		},
	}))
	_, err := ExportToStore(ctx, exportDB, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)

	note := "remember"
	writeMetadataArtifact(t, store, metadataEvent{
		Version:    formatVersion,
		HLC:        replayTestHLC(0, 0),
		Origin:     origin,
		SessionGID: gid,
		Op:         MetadataOpPin,
		Pin: &MetadataPin{
			SourceUUID: "uuid-answer",
			Ordinal:    1,
			Note:       &note,
		},
	})
	_, _, err = importFromTestStore(ctx, importDB, store, localOrigin)
	require.NoError(t, err)
	pins, err := importDB.ListPinnedMessages(ctx, gid, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, 1, pins[0].Ordinal)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, note, *pins[0].Note)

	writeMetadataArtifact(t, store, metadataEvent{
		Version:    formatVersion,
		HLC:        replayTestHLC(time.Nanosecond, 0),
		Origin:     origin,
		SessionGID: gid,
		Op:         MetadataOpUnpin,
		Pin: &MetadataPin{
			SourceUUID: "uuid-answer",
			Ordinal:    1,
		},
	})
	_, _, err = importFromTestStore(ctx, importDB, store, localOrigin)
	require.NoError(t, err)
	pins, err = importDB.ListPinnedMessages(ctx, gid, "")
	require.NoError(t, err)
	assert.Empty(t, pins)
	assert.Equal(t, 2, metadataAppliedCount(t, importDB, origin))
}

func TestLocalMetadataEditBeatsLowerHLCPeerEvent(t *testing.T) {
	ctx := context.Background()
	store := newTestArtifactStore(t)
	localOrigin := "desktop-d4e5f6"
	peerOrigin := "laptop-a1b2c3"
	gid := localOrigin + "~sess-1"

	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	// Simulate the local rename handler: mutate the session, then record the
	// metadata event artifact and replay register entry.
	require.NoError(t, database.RenameSession("sess-1", new("Local name")))

	now := fixedHLCTime().Add(time.Hour)
	recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Origin: localOrigin,
		Store:  store,
		Now:    func() time.Time { return now },
	})
	value, err := json.Marshal(struct {
		DisplayName string `json:"display_name"`
	}{DisplayName: "Local name"})
	require.NoError(t, err)
	localRec, err := recorder.Append(ctx, MetadataEventInput{
		SessionID: "sess-1",
		Op:        MetadataOpRename,
		Value:     value,
	})
	require.NoError(t, err)
	localOrderKey := localRec.HLC + "-" + localRec.Hash

	// A peer renames the same desktop-originated session at an earlier HLC.
	peerArt := writeMetadataArtifact(t, store,
		replayRenameEvent(t, peerOrigin, gid, replayTestHLC(0, 0), "Peer name"))
	require.Less(t, peerArt.orderKey, localOrderKey)

	res, err := ImportDetailedFromStore(ctx, database, store, localOrigin)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Metadata)

	got, err := database.GetSession(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, "Local name", *got.DisplayName)
	assertMetadataConflict(t, database, gid, "display_name", localOrderKey, peerArt.orderKey)
}

func TestImportContinuesPastUnavailableMetadataTarget(t *testing.T) {
	ctx := context.Background()
	store := newTestArtifactStore(t)
	localOrigin := "desktop-d4e5f6"
	peerOrigin := "laptop-a1b2c3"
	database := testDB(t)
	seedSession(t, database, "sess-present", "alpha")
	missingGID := localOrigin + "~sess-missing"
	presentGID := localOrigin + "~sess-present"

	// The earlier event targets a session that is not durable locally; the
	// later event targets an existing session and must still apply.
	writeMetadataArtifact(t, store,
		replayRenameEvent(t, peerOrigin, missingGID, replayTestHLC(0, 0), "Missing target"))
	writeMetadataArtifact(t, store,
		replayRenameEvent(t, peerOrigin, presentGID, replayTestHLC(time.Nanosecond, 0), "Present target"))

	res, err := ImportDetailedFromStore(ctx, database, store, localOrigin)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Metadata)

	got, err := database.GetSession(ctx, "sess-present")
	require.NoError(t, err)
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, "Present target", *got.DisplayName)
	// Only the present-target event is marked applied; the unavailable one is
	// left for a later run to retry.
	assert.Equal(t, 1, metadataAppliedCount(t, database, peerOrigin))

	// Once the missing session exists, a later import applies its event too.
	seedSession(t, database, "sess-missing", "beta")
	res, err = ImportDetailedFromStore(ctx, database, store, localOrigin)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Metadata)
	got, err = database.GetSession(ctx, "sess-missing")
	require.NoError(t, err)
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, "Missing target", *got.DisplayName)
	assert.Equal(t, 2, metadataAppliedCount(t, database, peerOrigin))
}

func TestReplayDefersRemoteEventBeyondClockDrift(t *testing.T) {
	ctx := context.Background()
	store := newTestArtifactStore(t)
	localOrigin := "desktop-d4e5f6"
	peerOrigin := "laptop-a1b2c3"
	gid := localOrigin + "~sess-1"

	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")

	now := fixedHLCTime()
	// A peer event whose wall time is an hour ahead — well beyond the default
	// 5-minute drift bound, so the local clock cannot be advanced past it.
	future := HLCTimestamp{WallTime: now.Add(time.Hour)}
	writeMetadataArtifact(t, store,
		replayRenameEvent(t, peerOrigin, gid, future.String(), "From the future"))

	recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Origin: localOrigin,
		Store:  store,
		Now:    func() time.Time { return now },
	})
	res, err := recorder.ImportFromStore(ctx, store)
	require.NoError(t, err)
	assert.Zero(t, res.Metadata, "event beyond the drift bound must be deferred, not applied")

	// The local session is untouched and the event is left unapplied for a later
	// run to retry once wall time catches up.
	got, err := database.GetSession(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.DisplayName)
	assert.NotEqual(t, "From the future", *got.DisplayName, "deferred rename must not be applied")
	assert.Equal(t, 0, metadataAppliedCount(t, database, peerOrigin))
}

func replayRenameEvent(
	t *testing.T,
	origin, gid, hlc, displayName string,
) metadataEvent {
	t.Helper()
	value, err := json.Marshal(struct {
		DisplayName string `json:"display_name"`
	}{
		DisplayName: displayName,
	})
	require.NoError(t, err)
	return metadataEvent{
		Version:    formatVersion,
		HLC:        hlc,
		Origin:     origin,
		SessionGID: gid,
		Op:         MetadataOpRename,
		Value:      value,
	}
}

func replayTestHLC(offset time.Duration, logical uint64) string {
	return HLCTimestamp{WallTime: fixedHLCTime().Add(offset), Logical: logical}.String()
}

func writeMetadataArtifact(t *testing.T, store ArtifactStore, event metadataEvent) metadataArtifact {
	t.Helper()
	stamp, err := ParseHLCTimestamp(event.HLC)
	require.NoError(t, err)
	data, err := canonicalJSON(event)
	require.NoError(t, err)
	hash := hashHex(data)
	orderKey := stamp.OrderingKey(hash)
	ref, err := NewRef(event.Origin, KindMeta, orderKey+metadataEventExtension)
	require.NoError(t, err)
	createContractArtifact(t, store, ref, data)
	return metadataArtifact{orderKey: orderKey, hash: hash, hlc: event.HLC, event: event}
}

func assertMetadataConflict(
	t *testing.T,
	database *db.DB,
	gid, field, wantWinning, wantLosing string,
) {
	t.Helper()
	var winning, losing string
	err := database.Reader().QueryRowContext(context.Background(),
		`SELECT winning_order_key, losing_order_key
		 FROM metadata_conflicts
		 WHERE session_gid = ? AND field = ?`,
		gid, field,
	).Scan(&winning, &losing)
	require.NoError(t, err)
	assert.Equal(t, wantWinning, winning)
	assert.Equal(t, wantLosing, losing)
}

func assertMetadataConflictCount(t *testing.T, database *db.DB, gid, field string, want int) {
	t.Helper()
	var got int
	err := database.Reader().QueryRowContext(context.Background(),
		`SELECT COUNT(*)
		 FROM metadata_conflicts
		 WHERE session_gid = ? AND field = ?`,
		gid, field,
	).Scan(&got)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func metadataAppliedCount(t *testing.T, database *db.DB, origin string) int {
	t.Helper()
	var count int
	err := database.Reader().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM metadata_applied_events WHERE origin = ?`,
		origin,
	).Scan(&count)
	require.NoError(t, err)
	return count
}
