package db_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func linkAt(fp, state string, next *time.Time) db.FrictionIssueLink {
	return db.FrictionIssueLink{
		Fingerprint: fp, State: state, NextAttemptAt: next,
		UpdatedAt: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
	}
}

func TestFrictionIssueLinksRoundTrip(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := t.Context()
	failedAt := time.Date(2026, 9, 20, 1, 2, 3, 0, time.UTC)
	next := failedAt.Add(time.Hour)
	in := db.FrictionIssueLink{
		Fingerprint: "fl1:aa", State: db.FrictionLinkStateFailed,
		KataInstanceUID: "inst", KataProjectUID: "proj", Attempts: 2,
		FirstFailedAt: &failedAt, NextAttemptAt: &next,
		LastErrorCode: "transport", LastError: "down", UpdatedAt: failedAt,
	}
	require.NoError(t, d.UpsertFrictionIssueLink(ctx, in))
	got, err := d.GetFrictionIssueLinks(ctx, []string{"fl1:aa", "fl1:missing"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, in, got["fl1:aa"])

	in.State, in.IssueUID, in.QualifiedID, in.LinkSource =
		db.FrictionLinkStateLinked, "01J0ABCDEF0000000000000001", "agentsview#f001", db.FrictionLinkSourceCreated
	in.FirstFailedAt, in.NextAttemptAt, in.LastErrorCode, in.LastError = nil, nil, "", ""
	require.NoError(t, d.UpsertFrictionIssueLink(ctx, in))
	got, err = d.GetFrictionIssueLinks(ctx, []string{"fl1:aa"})
	require.NoError(t, err)
	assert.Equal(t, in, got["fl1:aa"], "upsert replaces every column")

	require.NoError(t, d.DeleteFrictionIssueLink(ctx, "fl1:aa"))
	got, err = d.GetFrictionIssueLinks(ctx, []string{"fl1:aa"})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestFrictionIssueLinkRejectsUnknownState(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	err := d.UpsertFrictionIssueLink(t.Context(), linkAt("fl1:x", "bogus", nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown friction link state "bogus"`)
}

func TestDueFrictionFilings(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Minute), now.Add(time.Minute)
	for _, row := range []db.FrictionIssueLink{
		linkAt("fl1:pending-due", db.FrictionLinkStatePending, &past),
		linkAt("fl1:pending-null", db.FrictionLinkStatePending, nil),
		linkAt("fl1:failed-due", db.FrictionLinkStateFailed, &past),
		linkAt("fl1:failed-later", db.FrictionLinkStateFailed, &future),
		linkAt("fl1:needs-human", db.FrictionLinkStateNeedsHuman, &past),
		linkAt("fl1:abandoned", db.FrictionLinkStateAbandoned, &past),
		linkAt("fl1:linked", db.FrictionLinkStateLinked, nil),
	} {
		require.NoError(t, d.UpsertFrictionIssueLink(ctx, row))
	}
	for _, test := range []struct {
		name  string
		limit int
		want  []string
	}{
		{name: "all_due", limit: 50, want: []string{"fl1:pending-null", "fl1:failed-due", "fl1:pending-due"}},
		{name: "bounded", limit: 1, want: []string{"fl1:pending-null"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows, err := d.DueFrictionFilings(ctx, now, test.limit)
			require.NoError(t, err)
			var got []string
			for _, row := range rows {
				got = append(got, row.Fingerprint)
			}
			assert.Equal(t, test.want, got)
		})
	}
}
