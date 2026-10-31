package storage

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestCommitSnapshotPublishesLinksOnlyAndRollsBackFailure(t *testing.T) {
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	target, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "replica.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { target.Close() })
	_, err = target.ExecContext(ctx, CommitLinksDDL)
	require.NoError(t, err)
	require.NoError(t, local.StoreCommitLinks(ctx, []db.CommitLink{{SessionID: "unknown", CommitSHA: "abcdef0", Source: "trailer", Subject: "first"}}))
	require.NoError(t, PublishCommitLinksSQL(ctx, target, db.SQLiteQueryDialect(), local, nil, nil))
	rows, err := db.ReadCommitLinkPayloads(ctx, target.QueryContext, db.SQLiteQueryDialect(), CommitLinksRelation, []string{"abcdef0"}, "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "first", rows[0].Subject)
	require.NoError(t, local.StoreCommitLinks(ctx, []db.CommitLink{{SessionID: "unknown", CommitSHA: "abcdef0", Source: "trailer", Subject: "corrected"}, {SessionID: "another", CommitSHA: "deadbee", Source: "trailer"}}))
	_, err = target.ExecContext(ctx, `CREATE TRIGGER reject_fixture BEFORE INSERT ON commit_session_links WHEN NEW.commit_sha='deadbee' BEGIN SELECT RAISE(ABORT,'fixture constraint'); END`)
	require.NoError(t, err)
	assert.Error(t, PublishCommitLinksSQL(ctx, target, db.SQLiteQueryDialect(), local, nil, nil))
	rows, err = db.ReadCommitLinkPayloads(ctx, target.QueryContext, db.SQLiteQueryDialect(), CommitLinksRelation, []string{"abcdef0"}, "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "first", rows[0].Subject)
	_, err = target.ExecContext(ctx, "DROP TRIGGER reject_fixture")
	require.NoError(t, err)
	require.NoError(t, PublishCommitLinksSQL(ctx, target, db.SQLiteQueryDialect(), local, nil, nil))
	rows, err = db.ReadCommitLinkPayloads(ctx, target.QueryContext, db.SQLiteQueryDialect(), CommitLinksRelation, []string{"abcdef0", "deadbee"}, "")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "corrected", rows[0].Subject)
}

func TestCommitSnapshotPendingNeverLooksLikeMiss(t *testing.T) {
	ctx := t.Context()
	target, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "pending.sqlite"))
	require.NoError(t, err)
	defer target.Close()
	_, err = target.ExecContext(ctx, CommitLinksDDL)
	require.NoError(t, err)
	_, err = ReadPublishedCommitLinks(ctx, target.QueryContext, db.SQLiteQueryDialect(), CommitLinksRelation, "commit_link_publications", []string{"abcdef0"}, "")
	assert.ErrorIs(t, err, db.ErrCommitIndexUnavailable)
}

func TestCommitSnapshotUnfilteredPublicationSupersedesOlderFilteredScopes(t *testing.T) {
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "fixture", Agent: "codex", Project: "demo"}))
	require.NoError(t, local.StoreCommitLinks(ctx, []db.CommitLink{{SessionID: "fixture", CommitSHA: "abcdef0", Source: "trailer", Subject: "stale"}}))
	target, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "scope.sqlite"))
	require.NoError(t, err)
	defer target.Close()
	_, err = target.ExecContext(ctx, CommitLinksDDL)
	require.NoError(t, err)

	require.NoError(t, PublishCommitLinksSQL(ctx, target, db.SQLiteQueryDialect(), local, []string{"demo"}, nil))
	var ready bool
	require.NoError(t, target.QueryRowContext(ctx, "SELECT ready FROM commit_link_publications").Scan(&ready))
	assert.False(t, ready, "the filtered snapshot predates completion of backfill")

	require.NoError(t, local.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM commit_session_links")
		return err
	}))
	_, err = local.BackfillCommitLinks(ctx, 100)
	require.NoError(t, err)
	require.NoError(t, PublishCommitLinksSQL(ctx, target, db.SQLiteQueryDialect(), local, nil, nil))

	rows, err := ReadPublishedCommitLinks(ctx, target.QueryContext, db.SQLiteQueryDialect(), CommitLinksRelation, CurrentCommitLinkPublicationsRelation, []string{"abcdef0"}, "")
	require.NoError(t, err, "an older not-ready filtered scope cannot block the newer full snapshot")
	assert.Empty(t, rows, "removed evidence from the old filtered scope is no longer visible")
}

func TestCommitSnapshotFilteredPublicationSupersedesOlderUnfilteredScope(t *testing.T) {
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	require.NoError(t, local.StoreCommitLinks(ctx, []db.CommitLink{{
		SessionID: "unknown", CommitSHA: "abcdef0", Source: "trailer", Subject: "stale",
	}}))
	target, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "filtered-scope.sqlite"))
	require.NoError(t, err)
	defer target.Close()
	_, err = target.ExecContext(ctx, CommitLinksDDL)
	require.NoError(t, err)
	require.NoError(t, PublishCommitLinksSQL(ctx, target, db.SQLiteQueryDialect(), local, nil, nil))

	rows, err := ReadPublishedCommitLinks(ctx, target.QueryContext, db.SQLiteQueryDialect(), CommitLinksRelation, CurrentCommitLinkPublicationsRelation, []string{"abcdef0"}, "")
	require.NoError(t, err)
	require.Len(t, rows, 1, "the initial unfiltered publication exposes its evidence")

	require.NoError(t, local.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM commit_session_links")
		return err
	}))
	require.NoError(t, PublishCommitLinksSQL(ctx, target, db.SQLiteQueryDialect(), local, []string{"demo"}, nil))

	rows, err = ReadPublishedCommitLinks(ctx, target.QueryContext, db.SQLiteQueryDialect(), CommitLinksRelation, CurrentCommitLinkPublicationsRelation, []string{"abcdef0"}, "")
	require.NoError(t, err)
	assert.Empty(t, rows, "the filtered publication replaces stale evidence from the older full scope")
}

func TestCommitSnapshotPublishesNewNativeBindingWithoutTranscriptChange(t *testing.T) {
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	require.NoError(t, local.StoreCommitLinks(ctx, []db.CommitLink{{SessionID: "codex:native", CommitSHA: "abcdef0", Source: "trailer"}}))
	id := "one~codex:native"
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Agent: "codex", Project: "demo"}))
	_, err := local.BackfillCommitLinks(ctx, 100)
	require.NoError(t, err)
	target, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "binding.sqlite"))
	require.NoError(t, err)
	defer target.Close()
	_, err = target.ExecContext(ctx, CommitLinksDDL)
	require.NoError(t, err)
	require.NoError(t, PublishCommitLinksSQL(ctx, target, db.SQLiteQueryDialect(), local, nil, nil))
	root := db.RawArchiveRoot{ID: "fixture", DeviceID: "fixture", Machine: "fixture", Provider: "codex", OriginalPath: "/fixture", ConfiguredRootID: "fixture"}
	require.NoError(t, local.RegisterRawArchiveRoot(ctx, root))
	_, err = local.BindRawArchiveSession(ctx, root, "source", "codex:native", id)
	require.NoError(t, err)
	require.NoError(t, PublishCommitLinksSQL(ctx, target, db.SQLiteQueryDialect(), local, nil, nil))
	rows, err := db.ReadCommitLinkPayloads(ctx, target.QueryContext, db.SQLiteQueryDialect(), CommitLinksRelation, []string{"abcdef0"}, "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, id, rows[0].SessionID)
	assert.False(t, rows[0].Gone)
}
