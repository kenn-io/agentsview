//go:build chtest

package clickhouse

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestTrendsKeptPush(t *testing.T) {
	local, target := seedFixture(t)
	ctx := t.Context()
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	session, err := local.GetSession(ctx, fixtureBetaID)
	require.NoError(t, err)
	messages, err := local.GetAllMessages(ctx, fixtureBetaID)
	require.NoError(t, err)
	messages[0].Content = "alpha"
	messages[0].ContentLength = 5
	require.NoError(t, local.ReplaceSessionMessages(ctx, fixtureBetaID, messages))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	f := db.AnalyticsFilter{Project: session.Project, Timezone: "UTC", From: "2026-01-01", To: "2026-01-31"}
	terms := []db.TrendTermInput{{Term: "alpha", Variants: []string{"alpha"}, Matchers: []string{"alpha"}}}
	read := func(filter db.AnalyticsFilter) db.TrendsTermsResponse {
		r, err := store.GetTrendsTerms(ctx, filter, terms, "week")
		require.NoError(t, err)
		return r
	}
	first := read(f)
	require.Equal(t, 1, first.Series[0].Total)
	require.Equal(t, first, read(f))
	messages[0].Content = "bravo"
	require.NoError(t, local.ReplaceSessionMessages(ctx, fixtureBetaID, messages))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, read(f).Series[0].Total)
	session.Project = "changed-project"
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{Session: *session, Messages: messages, DataVersion: 1, ReplaceMessages: true}})
	require.NoError(t, err)
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, read(f).MessageCount)
	f.Project = "changed-project"
	require.Equal(t, 1, read(f).MessageCount)
	f.ActiveSince = "2000-01-01T00:00:00Z"
	require.Equal(t, 1, read(f).MessageCount)
	require.NoError(t, store.DB().Close())
	_, err = store.GetTrendsTerms(ctx, f, terms, "week")
	require.Error(t, err)
}

func TestTrendsKeptLocalTimezoneChange(t *testing.T) {
	localZone := time.Local //nolint:forbidigo // Exercise the process-local timezone.
	_ = time.Now().In(localZone)
	original := *localZone
	t.Cleanup(func() { *localZone = original })
	*localZone = *time.FixedZone("Local", 0)
	local, target := seedFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	f := db.AnalyticsFilter{From: "2026-01-10", To: "2026-01-11", Project: "beta", Timezone: "Local"}
	terms := []db.TrendTermInput{{Term: "beta", Variants: []string{"beta"}, Matchers: []string{"beta"}}}
	first, err := store.GetTrendsTerms(t.Context(), f, terms, "day")
	require.NoError(t, err)
	require.Equal(t, 1, first.Series[0].Points[1].Count)
	*localZone = *time.FixedZone("Local", -5*60*60)
	second, err := store.GetTrendsTerms(t.Context(), f, terms, "day")
	require.NoError(t, err)
	require.Equal(t, 1, second.Series[0].Points[0].Count)
	require.Zero(t, second.Series[0].Points[1].Count)
}
