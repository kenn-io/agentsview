//go:build duckdbtest && !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// Through Quack, parameters are rendered into the SQL text, so the marker
// patterns and the session ID list must survive literalization and judge
// prompts exactly as the local path does.
func TestQuackFrustrationMarkerScanCounts(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "agentsview-quack-frustration.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-frustration"
	ids := writeMarkerPrompts(t, path, []markerPrompt{
		{content: "why\n\twon't it build???"},
		{content: "WHY IS THIS FAILING NOW"},
		{content: "```\nWTF WTF WTF\n```\nplease look at it"},
		{content: "the outcome online, it's fine"},
		{content: "this iſ broken again"},
		{content: "come on, fix it"},
		{content: "this is broken again", isSystem: true},
	})
	openQuackMirrorServer(t, ctx, path, uri, token)
	store, err := NewQuackStore(ctx, uri, token, false, 0)
	require.NoError(t, err, "open Quack-backed store")
	t.Cleanup(func() { require.NoError(t, store.Close(), "close Quack-backed store") })

	assert.Equal(t, []int{1, 1, 0, 0, 1, 1, 0}, scanMarkers(t, store, ids))
	assert.Equal(t, map[string]bool{ids[4]: true, ids[5]: true}, shippedContent(t, store, ids))
}

func TestQuackFrustrationMirrorVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int
		want    int
		future  bool
	}{
		{"current", frustrationMarkersVersion, 1, false},
		{"stale", frustrationMarkersVersion - 1, 0, false},
		{"future", frustrationMarkersVersion + 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "mirror.duckdb")
			buildMirrorFixture(t, path, "session", "this is broken")
			conn, err := Open(ctx, path)
			require.NoError(t, err)
			_, err = conn.ExecContext(ctx, `UPDATE messages SET content = 'fine' WHERE session_id = 'session'`)
			require.NoError(t, err)
			require.NoError(t, recordMetadataKey(ctx, conn, frustrationMarkersVersionMetadataKey, strconv.Itoa(tc.version)))
			require.NoError(t, conn.Close())
			uri := "quack:127.0.0.1:" + freeTCPPort(t)
			const token = "agentsview-duckdbtest-token-frustration-version"
			openQuackMirrorServer(t, ctx, path, uri, token)
			store, err := NewQuackStore(ctx, uri, token, false, 0)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			resp, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
			if tc.future {
				require.ErrorContains(t, err, "newer than supported")
				empty, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{Project: "no-sessions"})
				require.NoError(t, err)
				assert.Zero(t, empty.ScoredSessions)
				assert.Zero(t, empty.UnscoredSessions)
				assert.Zero(t, empty.QualityHealth.Totals.FrustrationMarkerCount)
				_, err = store.GetAnalyticsSignalSessions(ctx, db.AnalyticsFilter{}, "frustration_marker_count", 10)
				require.ErrorContains(t, err, "newer than supported")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, resp.QualityHealth.Totals.FrustrationMarkerCount)
			evidence, err := store.GetAnalyticsSignalSessions(ctx, db.AnalyticsFilter{}, "frustration_marker_count", 10)
			require.NoError(t, err)
			assert.Len(t, evidence.Sessions, tc.want)
		})
	}
}
