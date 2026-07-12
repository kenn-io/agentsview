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
		FROM source_project_identity_observations
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
			SourceArchiveID:   "archive-batch",
			SourceArchiveSalt: "archive-batch-salt",
			Project:           "proj",
			Machine:           "m1",
			RootPath:          root,
			GitRemote:         remote,
			GitRemoteName:     name,
			ObservedAt:        time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			Key:               fmt.Sprintf("%s|%s", root, remote),
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
			`DELETE FROM source_project_identity_observations`)
		require.NoError(t, err)
		tx, err := pg.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		require.NoError(t, upsertSourceArchiveScope(
			ctx, tx, "archive-batch", "archive-batch-salt"))
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

func TestPGProjectIdentityAggregatesSourceArchives(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_project_identity_selector_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err)
	defer pg.Close()
	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(ctx, pg, schema))

	for i, archiveID := range []string{"archive-a", "archive-b"} {
		_, err = pg.ExecContext(ctx, `
			INSERT INTO source_archives (source_archive_id, source_archive_salt)
			VALUES ($1, $2)`, archiveID, archiveID+"-salt")
		require.NoError(t, err)
		_, err = pg.ExecContext(ctx, `
			INSERT INTO source_project_identity_observations (
				source_archive_id, source_archive_salt, project, machine,
				root_path, git_remote, observed_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			archiveID, archiveID+"-salt", "app", "host", "/repo/app",
			[]string{
				"https://github.com/acme/app-a.git",
				"https://github.com/acme/app-b.git",
			}[i],
			time.Date(2026, 7, 10, 12, i, 0, 0, time.UTC),
		)
		require.NoError(t, err)
	}

	store := &Store{pg: pg}
	projects, err := store.BuildProjectIdentityMap(ctx, []string{"app", "missing"})
	require.NoError(t, err)
	assert.Equal(t, export.ProjectResolutionAmbiguous, projects["app"].Resolution)
	assert.Nil(t, projects["app"].Identity)
	assert.NotEmpty(t, projects["app"].ProjectKey)
	assert.NotEmpty(t, projects["missing"].ProjectKey)
	assert.Contains(t, export.ProjectMapForWire(projects), projects["app"].ProjectKey)
	assert.Contains(t, export.ProjectMapForWire(projects), projects["missing"].ProjectKey)

}

func TestPGProjectIdentityLegacySessionsUseDistinctFallbackKeys(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_project_identity_legacy_scope_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err)
	defer pg.Close()
	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(ctx, pg, schema))
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (id, project, machine, agent)
		VALUES
			('legacy-alpha', 'alpha', 'host', 'codex'),
			('legacy-beta', 'beta', 'host', 'codex')`)
	require.NoError(t, err)

	store := &Store{pg: pg}
	first, err := store.BuildProjectIdentityMap(ctx, []string{"alpha", "beta"})
	require.NoError(t, err)
	second, err := store.BuildProjectIdentityMap(ctx, []string{"alpha", "beta"})
	require.NoError(t, err)

	assert.NotEmpty(t, first["alpha"].ProjectKey)
	assert.NotEmpty(t, first["beta"].ProjectKey)
	assert.NotEqual(t, first["alpha"].ProjectKey, first["beta"].ProjectKey)
	assert.Equal(t, first["alpha"].ProjectKey, second["alpha"].ProjectKey)
	assert.Equal(t, first["beta"].ProjectKey, second["beta"].ProjectKey)
	assert.Len(t, export.ProjectMapForWire(first), 2)
}

func TestPGSourceArchiveScopeRejectsSaltMismatch(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_project_identity_salt_mismatch_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err)
	defer pg.Close()
	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(ctx, pg, schema))

	require.NoError(t, upsertSourceArchiveScope(
		ctx, pg, "archive-a", "salt-a"))
	err = upsertSourceArchiveScope(ctx, pg, "archive-a", "salt-b")
	require.ErrorContains(t, err, "archive salt mismatch")
}
