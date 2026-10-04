//go:build pgtest

// internal/postgres/friction_links_pgtest_test.go
package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
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
		FirstFailedAt: &past, NextAttemptAt: &past, LastErrorCode: "transport", LastError: "down",
		CreateIdempotencyKey: "friction-force-new-retry", UpdatedAt: now}
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

func TestStoreDigestDatesForFingerprintsUsesFrozenSnapshots(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)
	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err)
	defer store.Close()
	ctx := t.Context()

	sig := friction.Signal{
		Kind: friction.KindError, SubjectID: "claude:removed", SubjectKind: friction.SubjectSession,
		ToolName: "Bash", Text: "failure from the frozen digest",
	}
	frozen, err := review.EncodeSnapshot(friction.DigestSnapshot{
		Date: "2026-09-15", Signals: []friction.Signal{sig},
	})
	require.NoError(t, err)
	for _, date := range []string{"2026-09-15", "2026-09-14", "2026-09-16"} {
		digest := db.FrictionDigest{
			Date: date, Timezone: "UTC", RulesVersion: friction.RulesVersion,
			BuiltAt: time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC), Revision: 1,
			SnapshotJSON: frozen, SummaryJSON: []byte("{}\n"), Markdown: []byte("# digest\n"),
			MarkdownSHA256: "sha", RunID: "run-" + date,
		}
		require.NoError(t, store.SaveFrictionDigest(ctx, digest, nil, nil))
	}
	require.NoError(t, store.SaveFrictionDigest(ctx, db.FrictionDigest{
		Date: "2026-09-17", Timezone: "UTC", RulesVersion: friction.RulesVersion,
		BuiltAt: time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC), Revision: 1,
		SnapshotJSON: []byte(`{"signals":[]}`), SummaryJSON: []byte("{}\n"),
		Markdown: []byte("# digest\n"), MarkdownSHA256: "sha", RunID: "run-empty",
	}, nil, nil))
	emptyDates, err := store.DigestDatesForFingerprints(ctx, []string{""})
	require.NoError(t, err)
	assert.Empty(t, emptyDates)
	var indexed int
	require.NoError(t, store.pg.QueryRowContext(ctx,
		`SELECT count(*) FROM friction_digest_fingerprints WHERE fingerprint = $1`, sig.Fingerprint()).Scan(&indexed))
	assert.Equal(t, 3, indexed)
	_, err = store.pg.ExecContext(ctx,
		`DELETE FROM friction_digest_fingerprints WHERE date = $1`, "2026-09-16")
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(ctx, store.pg, testSchema))
	_, err = store.pg.ExecContext(ctx, `UPDATE friction_digests SET snapshot_json = '{"signals":[]}'`)
	require.NoError(t, err)

	got, err := store.DigestDatesForFingerprints(ctx, []string{sig.Fingerprint(), ""})
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{
		sig.Fingerprint(): {"2026-09-14", "2026-09-15", "2026-09-16"},
	}, got)
}
