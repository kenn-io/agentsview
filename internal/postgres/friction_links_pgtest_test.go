//go:build pgtest

// internal/postgres/friction_links_pgtest_test.go
package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestStoreFrictionIssueLinksParity(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)
	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err)
	defer store.Close()
	ctx := t.Context()
	_, err = store.DB().ExecContext(ctx, `DELETE FROM friction_issue_links`)
	require.NoError(t, err)

	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	in := db.FrictionIssueLink{Fingerprint: "fl1:aa", State: db.FrictionLinkStateFailed, Attempts: 1,
		FirstFailedAt: &past, NextAttemptAt: &past, LastErrorCode: "transport", LastError: "down", UpdatedAt: now}
	require.NoError(t, store.UpsertFrictionIssueLink(ctx, in))
	require.NoError(t, store.UpsertFrictionIssueLink(ctx, db.FrictionIssueLink{Fingerprint: "fl1:nh", State: db.FrictionLinkStateNeedsHuman, UpdatedAt: now}))
	require.Error(t, store.UpsertFrictionIssueLink(ctx, db.FrictionIssueLink{Fingerprint: "fl1:x", State: "bogus"}))

	got, err := store.GetFrictionIssueLinks(ctx, []string{"fl1:aa"})
	require.NoError(t, err)
	assert.Equal(t, in, got["fl1:aa"])

	due, err := store.DueFrictionFilings(ctx, now, 50)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, "fl1:aa", due[0].Fingerprint)

	require.NoError(t, store.DeleteFrictionIssueLink(ctx, "fl1:aa"))
	got, err = store.GetFrictionIssueLinks(ctx, []string{"fl1:aa"})
	require.NoError(t, err)
	assert.Empty(t, got)
}
