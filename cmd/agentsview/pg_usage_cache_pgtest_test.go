//go:build pgtest

package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPGServeStartsUsageCacheWarm(t *testing.T) {
	pgURL := os.Getenv("TEST_PG_URL")
	if pgURL == "" {
		t.Skip("TEST_PG_URL must point to a dedicated test database")
	}
	ctx := t.Context()
	schema := fmt.Sprintf("usage_launch_%x", rand.Uint64())
	local, err := db.Open(ctx, filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close()) })
	syncer, err := postgres.New(ctx, pgURL, schema, local, "test-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	require.NoError(t, syncer.EnsureSchema(ctx))
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "session", Project: "project", Machine: "test-machine", Agent: "codex"}))
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	store, err := postgres.NewStore(ctx, pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := store.DB().ExecContext(context.WithoutCancel(ctx), `DROP SCHEMA `+schema+` CASCADE`)
		require.NoError(t, err)
		require.NoError(t, store.Close())
	})
	cfg := config.Config{DataDir: t.TempDir()}
	_, cleanup, err := (pgReplica{}).serveOptions(ctx, cfg, storage.ReplicaTarget{URL: pgURL, Schema: schema}, store)
	require.NoError(t, err)
	if cleanup != nil {
		t.Cleanup(func() { require.NoError(t, cleanup()) })
	}
	require.NoError(t, store.WaitUsageCacheWarm(ctx))
	files, err := filepath.Glob(filepath.Join(cfg.DataDir, "usage-postgres", "*", "usage-cache-v*.db"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	cache, err := sql.Open("sqlite3", files[0])
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cache.Close()) })
	var ready int
	require.NoError(t, cache.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_rollup_timezones WHERE change_revision IS NOT NULL`).Scan(&ready))
	require.Equal(t, 1, ready)
}
