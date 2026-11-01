//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestCommitLinksMirrorPublishesWithoutSessionCandidates(t *testing.T) {
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	require.NoError(t, local.StoreCommitLinks(ctx, []db.CommitLink{{SessionID: "missing", CommitSHA: "abcdef0", Source: "trailer", Subject: "first"}}))
	_, err := Push(ctx, path, local, "fixture", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	read := func(subject string) {
		t.Helper()
		conn, err := Open(ctx, path)
		require.NoError(t, err)
		defer conn.Close()
		rows, err := NewStoreFromDB(conn).CommitLinks(ctx, []string{"abcdef0"}, "")
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, subject, rows[0].Subject)
		assert.True(t, rows[0].Gone)
	}
	read("first")
	require.NoError(t, local.StoreCommitLinks(ctx, []db.CommitLink{{SessionID: "missing", CommitSHA: "abcdef0", Source: "trailer", Subject: "corrected"}}))
	_, err = Push(ctx, path, local, "fixture", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	read("corrected")
}
