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
		{"pending archive", dataVersion - 1, true, (1 << 20) + dataVersion - 1, true},
		{"current archive", dataVersion, true, (1 << 20) + dataVersion, false},
		{"ordinary archive", dataVersion, false, dataVersion, false},
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
	assert.Equal(t, (1<<20)+dataVersion, raw)
}
