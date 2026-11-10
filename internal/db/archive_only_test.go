package db

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveOnlyPreservesPendingRebuild(t *testing.T) {
	for _, tc := range []struct {
		name        string
		version     int
		archiveOnly bool
		wantRaw     int
		stale       bool
	}{
		{"pending archive", dataVersion - 1, true, (1 << 21) + (1 << 20) + dataVersion - 1, true},
		{"current archive", dataVersion, true, (1 << 21) + (1 << 20) + dataVersion, false},
		{"ordinary archive", dataVersion, false, (1 << 21) + dataVersion, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", tc.version))
				return err
			}))
			if tc.archiveOnly {
				require.NoError(t, d.EnableArchiveOnly(ctx))
				require.NoError(t, d.EnableArchiveOnly(ctx))
			}
			path := d.Path()
			require.NoError(t, d.Close())
			reopened, err := OpenIsolatedContext(ctx, path)
			require.NoError(t, err)
			defer reopened.Close()
			assert.Equal(t, tc.stale, reopened.NeedsResync())
			var raw int
			require.NoError(t, reopened.Reader().QueryRow(ctx, "PRAGMA user_version").Scan(&raw))
			assert.Equal(t, tc.wantRaw, raw)
			if tc.archiveOnly {
				require.ErrorIs(t, reopened.RequireSourceSync(ctx), ErrArchiveOnly)
				assert.Greater(t, raw, dataVersion, "readers without archive-only support reject the encoded version")
			} else {
				assert.NoError(t, reopened.RequireSourceSync(ctx))
			}
		})
	}
}

func TestArchiveOnlySurvivesRebuildMetadataCopy(t *testing.T) {
	source := testDB(t)
	ctx := t.Context()
	require.NoError(t, source.EnableArchiveOnly(ctx))
	replacement := testDB(t)
	require.NoError(t, replacement.CopySessionMetadataFrom(t.Context(), source.Path()))
	require.NoError(t, replacement.setDataVersion(ctx))
	path := replacement.Path()
	require.NoError(t, replacement.Close())
	reopened, err := OpenReadOnly(ctx, path)
	require.NoError(t, err)
	defer reopened.Close()
	assert.False(t, reopened.NeedsResync())
	require.ErrorIs(t, reopened.RequireSourceSync(ctx), ErrArchiveOnly)
	var raw int
	require.NoError(t, reopened.Reader().QueryRow(ctx, "PRAGMA user_version").Scan(&raw))
	assert.Equal(t, (1<<21)+(1<<20)+dataVersion, raw)
}

func TestDecodeUserVersionArchiveBits(t *testing.T) {
	for _, tc := range []struct {
		raw, version int
		archiveOnly  bool
	}{
		{169, 169, false},
		{1<<21 | 169, 169, false},
		{1<<20 | 169, 169, true},
		{1<<21 | 1<<20 | 168, 168, true},
		{1<<22 | 1<<21 | 169, 1<<22 | 169, false},
	} {
		version, archiveOnly := DecodeUserVersion(tc.raw)
		assert.Equal(t, tc.version, version)
		assert.Equal(t, tc.archiveOnly, archiveOnly)
	}
	// The default branch subtracts only the archive-only bit before comparing.
	for _, raw := range []int{1<<21 | 169, 1<<21 | 1<<20 | 169} {
		oldVersion := raw
		if oldVersion >= 1<<20 {
			oldVersion -= 1 << 20
		}
		assert.Greater(t, oldVersion, 169)
	}
}

func TestOpenRejectsUnknownArchiveFormat(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	path := d.Path()
	const rawVersion = 1<<22 | 1<<21 | 169
	_, err := d.getWriter().ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", rawVersion))
	require.NoError(t, err)
	require.NoError(t, d.Close())
	_, err = Open(ctx, path)
	require.True(t, IsDataVersionTooNew(err))
	raw, err := sql.Open(sqliteArchiveDriverName, makeDSN(path, true))
	require.NoError(t, err)
	defer raw.Close()
	var version int
	require.NoError(t, raw.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version))
	require.Equal(t, rawVersion, version)
}
