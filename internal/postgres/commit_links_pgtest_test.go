//go:build pgtest

package postgres

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestCommitLinksPostgresPublicationParity(t *testing.T) {
	pgURL := os.Getenv("TEST_PG_URL")
	if pgURL == "" {
		t.Skip("TEST_PG_URL must name a dedicated test database")
	}
	ctx := t.Context()
	target, err := Open(pgURL, "agentsview_commit_links_test", true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	_, err = target.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS agentsview_commit_links_test")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := target.Exec("DROP SCHEMA agentsview_commit_links_test CASCADE")
		require.NoError(t, err)
	})
	_, err = target.ExecContext(ctx, storage.CommitLinksDDL)
	require.NoError(t, err)
	_, err = target.ExecContext(ctx, `CREATE TABLE sessions(id TEXT PRIMARY KEY,source_archive_id TEXT,deleted_at TIMESTAMPTZ)`)
	require.NoError(t, err)
	local := dbtest.OpenTestDB(t)
	require.NoError(t, local.StoreCommitLinks(ctx, []db.CommitLink{{SessionID: "missing", CommitSHA: "abcdef0", Source: "trailer", Subject: "first"}}))
	for _, subject := range []string{"first", "corrected"} {
		require.NoError(t, local.StoreCommitLinks(ctx, []db.CommitLink{{SessionID: "missing", CommitSHA: "abcdef0", Source: "trailer", Subject: subject}}))
		require.NoError(t, storage.PublishCommitLinksSQL(ctx, target, db.PostgresQueryDialect(), local, nil, nil))
		rows, err := (&Store{pg: target}).CommitLinks(ctx, []string{"ABCDEF0"}, "")
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, subject, rows[0].Subject)
		assert.True(t, rows[0].Gone)
	}
}

func TestCommitLinksPostgresCurationClearsOwnedContext(t *testing.T) {
	pgURL := os.Getenv("TEST_PG_URL")
	if pgURL == "" {
		t.Skip("dedicated TEST_PG_URL required")
	}
	ctx := t.Context()
	target, err := Open(pgURL, "agentsview_commit_curation_test", true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	_, err = target.ExecContext(ctx, `CREATE SCHEMA agentsview_commit_curation_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := target.Exec(`DROP SCHEMA agentsview_commit_curation_test CASCADE`)
		require.NoError(t, err)
	})
	_, err = target.ExecContext(ctx, storage.CommitLinksDDL+`;CREATE TABLE sessions(id TEXT PRIMARY KEY,source_archive_id TEXT,deleted_at TIMESTAMPTZ)`)
	require.NoError(t, err)
	local := dbtest.OpenTestDB(t)
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "fixture", Agent: "claude", Project: "demo"}))
	require.NoError(t, local.ReplaceSessionMessages(ctx, "fixture", []db.Message{{SessionID: "fixture", Ordinal: 0, Role: "user", Content: "private prompt", Timestamp: "2026-01-01T00:00:00Z"}}))
	_, err = local.BackfillCommitLinks(ctx, 100)
	require.NoError(t, err)
	require.NoError(t, local.StoreCommitLinks(ctx, []db.CommitLink{{SessionID: "fixture", CommitSHA: "abcdef0", Source: "trailer", CommittedAt: "2026-01-01T00:01:00Z"}}))
	require.NoError(t, storage.PublishCommitLinksSQL(ctx, target, db.PostgresQueryDialect(), local, nil, nil))
	archive, err := local.GetArchiveID(ctx)
	require.NoError(t, err)
	_, err = target.ExecContext(ctx, `INSERT INTO sessions VALUES('fixture',$1,NULL)`, archive)
	require.NoError(t, err)
	store := &Store{pg: target}
	rows, err := store.CommitLinks(ctx, []string{"abcdef0"}, "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Turn)
	_, err = target.ExecContext(ctx, `UPDATE sessions SET deleted_at=NOW() WHERE id='fixture'`)
	require.NoError(t, err)
	rows, err = store.CommitLinks(ctx, []string{"abcdef0"}, "")
	require.NoError(t, err)
	assert.Empty(t, rows)
	tx, err := target.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = deletePGTrashedSessionRows(ctx, tx, []string{"fixture"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	rows, err = store.CommitLinks(ctx, []string{"abcdef0"}, "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].Gone)
	assert.Nil(t, rows[0].Turn)
	assert.Empty(t, rows[0].Agent)
	require.NoError(t, local.StoreCommitLinks(ctx, []db.CommitLink{{SessionID: "fixture", CommitSHA: "abcdef0", Source: "trailer", Subject: "unrelated revision", CommittedAt: "2026-01-01T00:01:00Z"}}))
	require.NoError(t, storage.PublishCommitLinksSQL(ctx, target, db.PostgresQueryDialect(), local, nil, nil, sanitizeCommitPublicationPG))
	var retained string
	require.NoError(t, target.QueryRowContext(ctx, "SELECT payload FROM commit_session_links WHERE session_id='fixture'").Scan(&retained))
	assert.NotContains(t, retained, "private prompt")
	rows, err = store.CommitLinks(ctx, []string{"abcdef0"}, "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].Gone)
	assert.Nil(t, rows[0].Turn)
}
