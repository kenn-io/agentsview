package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// importFromTestStore exercises the production exact-reference importer with
// the newest checkpoint in a test fixture. Initial transports report this same
// reference after transferring its dependencies.
func importFromTestStore(
	ctx context.Context, database *db.DB, store ArtifactStore, localOrigin string,
) (int, int, error) {
	result, err := importResultFromTestStore(ctx, database, store, localOrigin)
	return result.Sessions, result.Messages, err
}

func importResultFromTestStore(
	ctx context.Context, database *db.DB, store ArtifactStore, localOrigin string,
) (ImportResult, error) {
	origins, err := store.Origins(ctx)
	if err != nil {
		return ImportResult{}, err
	}
	defer origins.Close()
	coordinator := NewStoreImportCoordinator(database, store, localOrigin)
	for {
		page, nextErr := origins.Next(ctx, artifactImportPageSize)
		for _, origin := range page {
			if origin == localOrigin {
				continue
			}
			for _, kind := range []Kind{KindCheckpoints, KindMeta} {
				entries, err := testStoreEntries(ctx, store, origin, kind)
				if err != nil {
					return ImportResult{}, err
				}
				if kind == KindCheckpoints && len(entries) > 1 {
					entries = entries[len(entries)-1:]
				}
				for _, entry := range entries {
					if err := coordinator.RecordChanged(ctx, entry); err != nil {
						return ImportResult{}, err
					}
				}
			}
		}
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return ImportResult{}, nextErr
		}
	}
	return coordinator.Finalize(ctx)
}

func testStoreEntries(
	ctx context.Context, store ArtifactStore, origin string, kind Kind,
) ([]Entry, error) {
	iterator, err := store.Entries(ctx, origin, kind)
	if err != nil {
		return nil, err
	}
	defer iterator.Close()
	var entries []Entry
	for {
		page, nextErr := iterator.Next(ctx, artifactImportPageSize)
		entries = append(entries, page...)
		if errors.Is(nextErr, io.EOF) {
			return entries, nil
		}
		if nextErr != nil {
			return nil, nextErr
		}
	}
}

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

func replayRenameEvent(
	t *testing.T, origin, gid, hlc, displayName string,
) metadataEvent {
	t.Helper()
	value, err := json.Marshal(struct {
		DisplayName string `json:"display_name"`
	}{DisplayName: displayName})
	require.NoError(t, err)
	return metadataEvent{
		Version: formatVersion, HLC: hlc, Origin: origin,
		SessionGID: gid, Op: MetadataOpRename, Value: value,
	}
}

func replayTestHLC(offset time.Duration, logical uint64) string {
	return HLCTimestamp{WallTime: fixedHLCTime().Add(offset), Logical: logical}.String()
}

func assertMetadataConflictCount(t *testing.T, database *db.DB, gid, field string, want int) {
	t.Helper()
	var got int
	err := database.Reader().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM metadata_conflicts WHERE session_gid = ? AND field = ?`,
		gid, field,
	).Scan(&got)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
