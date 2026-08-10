package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestCollectLiveActivityTargetsUsesOnlyConfiguredHintProviders(t *testing.T) {
	base := t.TempDir()
	custom := filepath.Join(t.TempDir(), "custom")
	cfg := config.Config{
		LocalMachineName: "local",
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {
				filepath.Join(base, "sessions"),
				filepath.Join(base, "archived_sessions"),
				filepath.Join(custom, "sessions"),
				"s3://bucket/archive/sessions",
			},
			parser.AgentClaude: {filepath.Join(t.TempDir(), "claude")},
		},
	}

	targets, err := collectLiveActivityTargets(t.Context(), cfg)

	require.NoError(t, err)
	require.Len(t, targets, 1)
	assert.Equal(t, parser.AgentCodex, targets[0].Provider.Definition().Type)
	assert.Equal(t, []parser.ActivityHintSource{
		{Path: filepath.Join(base, "history.jsonl")},
		{Path: filepath.Join(custom, "history.jsonl")},
	}, targets[0].Sources)
}

// TestCollectLiveActivityTargetsIncludesTraeX pins the TraeX hint wiring:
// TRAE CLI writes history.jsonl at the same position relative to its sessions
// root as Codex, so it reaches the poller with its own traex: ID prefix.
func TestCollectLiveActivityTargetsIncludesTraeX(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".trae", "cli")
	cfg := config.Config{
		LocalMachineName: "local",
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentTraeX: {filepath.Join(base, "sessions")},
		},
	}

	targets, err := collectLiveActivityTargets(t.Context(), cfg)

	require.NoError(t, err)
	require.Len(t, targets, 1)
	assert.Equal(t, parser.AgentTraeX, targets[0].Provider.Definition().Type)
	assert.Equal(t, "traex:", targets[0].Provider.Definition().IDPrefix)
	assert.Equal(t, []parser.ActivityHintSource{
		{Path: filepath.Join(base, "history.jsonl")},
	}, targets[0].Sources)
}

func TestCollectLiveActivityTargetsDoesNotRequireExistingRoots(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "missing", "sessions")
	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {missing},
		},
	}

	targets, err := collectLiveActivityTargets(t.Context(), cfg)

	require.NoError(t, err)
	require.Len(t, targets, 1)
	assert.Equal(t, filepath.Join(base, "missing", "history.jsonl"),
		targets[0].Sources[0].Path)
	_, statErr := os.Stat(missing)
	assert.ErrorIs(t, statErr, os.ErrNotExist,
		"target collection must not create or discover rollout roots")
}

func TestLiveActivityIndexedLookupReturnsExactStoredMetadata(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	size := int64(123)
	mtime := int64(456)
	inode := int64(789)
	device := int64(1011)
	require.NoError(t, database.UpsertSession(db.Session{
		ID:         "codex:exact-id",
		Project:    "project",
		Machine:    "local",
		Agent:      string(parser.AgentCodex),
		FilePath:   &path,
		FileSize:   &size,
		FileMtime:  &mtime,
		FileInode:  &inode,
		FileDevice: &device,
	}))
	lookup := newLiveActivityLookup(database)

	got, found, err := lookup(t.Context(), "codex:exact-id")

	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, agentsync.LiveActivitySource{
		Path:              path,
		StoredSize:        size,
		StoredMTimeNS:     mtime,
		StoredInode:       inode,
		StoredDevice:      device,
		HasStoredStat:     true,
		HasStoredIdentity: true,
	}, got)

	_, found, err = lookup(t.Context(), "codex:missing-id")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestLiveActivityIndexedLookupSchedulesRowsWithoutCompleteStat(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, database.UpsertSession(db.Session{
		ID:       "codex:no-stat",
		Project:  "project",
		Machine:  "local",
		Agent:    string(parser.AgentCodex),
		FilePath: &path,
	}))

	got, found, err := newLiveActivityLookup(database)(
		t.Context(), "codex:no-stat",
	)

	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, path, got.Path)
	assert.False(t, got.HasStoredStat)
}

func TestStartLiveActivityRunTracksSyncAndWaitsForStop(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	base := t.TempDir()
	sessions := filepath.Join(base, "sessions")
	history := filepath.Join(base, "history.jsonl")
	rollout := filepath.Join(base, "rollout.jsonl")
	id := "019f0000-0000-7000-8000-000000000002"
	now := time.Now()
	require.NoError(t, os.WriteFile(history, []byte(
		`{"session_id":"`+id+`","ts":`+
			strconv.FormatInt(now.Unix(), 10)+
			`,"text":"private prompt sentinel"}`+"\n",
	), 0o644))
	require.NoError(t, os.WriteFile(rollout, []byte("changed"), 0o644))
	provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{
		Roots: []string{sessions},
	})
	require.True(t, ok)
	hints, ok, err := parser.ResolveActivityHintProvider(provider)
	require.NoError(t, err)
	require.True(t, ok)
	sources, err := hints.ActivityHintSources(t.Context())
	require.NoError(t, err)

	idled := make(chan struct{}, 1)
	idle := server.NewIdleTracker(20*time.Millisecond, func() {
		idled <- struct{}{}
	})
	go idle.Run(ctx)

	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	trackedSync := trackLiveActivitySync(idle,
		func(context.Context, []string) error {
			close(entered)
			<-release
			close(finished)
			return nil
		})
	runCtx, runCancel := context.WithCancel(ctx)
	poller := agentsync.NewLiveActivityPoller(
		[]agentsync.LiveActivityTarget{{
			Provider: provider,
			Hints:    hints,
			Sources:  sources,
		}},
		func(context.Context, string) (agentsync.LiveActivitySource, bool, error) {
			return agentsync.LiveActivitySource{Path: rollout}, true, nil
		},
		trackedSync,
		nil,
	)
	stop := startLiveActivityRun(runCtx, runCancel, poller)

	select {
	case <-entered:
	case <-time.After(time.Second):
		require.FailNow(t, "tracked sync did not start")
	}
	select {
	case <-idled:
		require.FailNow(t, "idle callback fired while sync work was active")
	case <-time.After(3 * 20 * time.Millisecond):
	}

	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		require.FailNow(t, "stop returned before active sync work completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		require.FailNow(t, "tracked sync did not finish")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		require.FailNow(t, "stop did not join the poller goroutine")
	}
}
