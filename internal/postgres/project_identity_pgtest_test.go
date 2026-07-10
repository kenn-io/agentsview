//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
)

type identityRow struct {
	Project       string
	Machine       string
	RootPath      string
	GitRemote     string
	GitRemoteName string
	Key           string
}

func readIdentityRows(
	t *testing.T, ctx context.Context, pg *sql.DB,
) []identityRow {
	t.Helper()
	rows, err := pg.QueryContext(ctx, `
		SELECT project, machine, root_path, git_remote, git_remote_name, key
		FROM project_identity_observations
		ORDER BY project, machine, root_path, git_remote`)
	require.NoError(t, err)
	defer rows.Close()
	var out []identityRow
	for rows.Next() {
		var r identityRow
		require.NoError(t, rows.Scan(
			&r.Project, &r.Machine, &r.RootPath,
			&r.GitRemote, &r.GitRemoteName, &r.Key,
		))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

// TestSyncProjectIdentityObservationsBatchMatchesSequential pins that the
// set-based observation sync leaves the table in exactly the state the
// per-row upsert loop produced, across the fallback-vs-real-remote cases
// the per-row logic encodes.
func TestSyncProjectIdentityObservationsBatchMatchesSequential(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_projident_batch_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	obs := func(root, remote, name string) export.ProjectIdentityObservation {
		return export.ProjectIdentityObservation{
			Project:       "proj",
			Machine:       "m1",
			RootPath:      root,
			GitRemote:     remote,
			GitRemoteName: name,
			ObservedAt:    time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			Key:           fmt.Sprintf("%s|%s", root, remote),
		}
	}

	// Pre-existing PG state the batch must interact with: a fallback row
	// that a batched real observation must delete, a real-remote row that
	// must suppress a batched fallback, and a real-remote row the batch
	// updates via ON CONFLICT.
	preExisting := []export.ProjectIdentityObservation{
		obs("/replaced-fallback", "", ""),
		obs("/has-remote", "git@x:h.git", "origin"),
		obs("/updated", "git@x:u.git", "old-name"),
	}

	// The batch covers: real+fallback for one root in both orders, a
	// surviving fallback, a fallback suppressed by pre-existing state, a
	// real row replacing a pre-existing fallback, an ON CONFLICT update,
	// and duplicate conflict keys where the last observation wins.
	batch := []export.ProjectIdentityObservation{
		obs("/mixed-real-first", "git@x:m1.git", "origin"),
		obs("/mixed-real-first", "", ""),
		obs("/mixed-fallback-first", "", ""),
		obs("/mixed-fallback-first", "git@x:m2.git", "origin"),
		obs("/fallback-only", "", ""),
		obs("/has-remote", "", ""),
		obs("/replaced-fallback", "git@x:r.git", "origin"),
		obs("/updated", "git@x:u.git", "dup-old"),
		obs("/updated", "git@x:u.git", "new-name"),
	}

	reset := func() {
		_, err := pg.ExecContext(ctx,
			`DELETE FROM project_identity_observations`)
		require.NoError(t, err)
		tx, err := pg.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		for _, o := range preExisting {
			require.NoError(t,
				upsertProjectIdentityObservation(ctx, tx, o, ""))
		}
		require.NoError(t, tx.Commit())
	}

	reset()
	tx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	for _, o := range batch {
		require.NoError(t, upsertProjectIdentityObservation(ctx, tx, o, ""))
	}
	require.NoError(t, tx.Commit())
	sequential := readIdentityRows(t, ctx, pg)

	reset()
	tx, err = pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, syncProjectIdentityObservationsBatch(ctx, tx, batch))
	require.NoError(t, tx.Commit())
	batched := readIdentityRows(t, ctx, pg)

	assert.Equal(t, sequential, batched,
		"batched sync must leave the table in the sequential state")

	// Anchor the shared outcome so a bug that changes both paths in the
	// same way cannot slip through the differential comparison.
	assert.Equal(t, []identityRow{
		{"proj", "m1", "/fallback-only", "", "", "/fallback-only|"},
		{"proj", "m1", "/has-remote", "git@x:h.git", "origin",
			"/has-remote|git@x:h.git"},
		{"proj", "m1", "/mixed-fallback-first", "git@x:m2.git", "origin",
			"/mixed-fallback-first|git@x:m2.git"},
		{"proj", "m1", "/mixed-real-first", "git@x:m1.git", "origin",
			"/mixed-real-first|git@x:m1.git"},
		{"proj", "m1", "/replaced-fallback", "git@x:r.git", "origin",
			"/replaced-fallback|git@x:r.git"},
		{"proj", "m1", "/updated", "git@x:u.git", "new-name",
			"/updated|git@x:u.git"},
	}, batched)

	// An empty batch must be a no-op rather than an SQL error.
	tx, err = pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, syncProjectIdentityObservationsBatch(ctx, tx, nil))
	require.NoError(t, tx.Commit())
	assert.Equal(t, batched, readIdentityRows(t, ctx, pg))
}
