//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPushFrustrationMarkerDefinitionVersion1(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	const ts = "2026-02-01T09:00:00.000Z"
	cases := []struct {
		id, role, content, subtype string
		system                     bool
		want                       int
	}{
		{"phrase", "user", "this is broken after the retry", "", false, 1},
		{"assistant", "assistant", "this is broken after the retry", "", false, 0},
		{"system", "user", "this is broken after the retry", "", true, 0},
		{"tool-result", "user", "this is broken after the retry", "tool_result", false, 0},
	}
	writes := make([]db.SessionBatchWrite, 0, len(cases))
	for _, tc := range cases {
		msg := syncMessage(tc.id, 0, tc.role, tc.content, ts)
		msg.IsSystem, msg.SourceSubtype = tc.system, tc.subtype
		writes = append(writes, db.SessionBatchWrite{
			Session:  syncSession(tc.id, "alpha", tc.content, ts, 1),
			Messages: []db.Message{msg}, DataVersion: 1, ReplaceMessages: true,
		})
	}
	_, err := local.WriteSessionBatchAtomic(ctx, writes)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	conn, err := OpenReadOnly(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	version, err := readMetadataKey(ctx, conn, frustrationMarkersVersionMetadataKey)
	require.NoError(t, err)
	// Update these expectations with the version when the counting definition changes.
	require.Equal(t, "1", version)
	for _, tc := range cases {
		var count int
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT frustration_marker_count FROM sessions WHERE id = ?`, tc.id).Scan(&count))
		assert.Equal(t, tc.want, count, tc.id)
	}
}
