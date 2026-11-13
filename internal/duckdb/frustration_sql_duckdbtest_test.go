//go:build duckdbtest && !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
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

func TestQuackStoredAndStaleFrustrationCounts(t *testing.T) {
	ctx := t.Context()
	local, path := newPushFixture(t, 2)
	appendFrustrationPrompt(t, local, "sess-1")
	appendFrustrationPrompt(t, local, "sess-2")
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	conn, err := Open(ctx, path)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE messages SET content = 'fine' WHERE role = 'user'`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET project = id, frustration_markers_version = CASE WHEN id = 'sess-2' THEN 0 ELSE frustration_markers_version END`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-stored-frustration"
	openQuackMirrorServer(t, ctx, path, uri, token)
	store, err := NewQuackStore(ctx, uri, token, false, 0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	for _, tc := range []struct {
		project string
		want    int
	}{{"sess-1", 1}, {"sess-2", 0}} {
		resp, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{Project: tc.project})
		require.NoError(t, err)
		assert.Equal(t, tc.want, resp.QualityHealth.Totals.FrustrationMarkerCount)
	}
}
