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
		{"pending archive", 127, true, 1048703, true},
		{"current archive", 128, true, 1048704, false},
		{"ordinary upgrade", 128, false, 128, false},
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
				assert.Greater(t, raw, 128, "the pre-merge reader rejects user_version above 128")
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
	require.NoError(t, replacement.CopySessionMetadataFrom(source.Path()))
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
	assert.Equal(t, 1048704, raw)
}
