package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type countingArtifactWatchSyncer struct {
	syncAllCalls int
	flushCalls   int
}

func (s *countingArtifactWatchSyncer) SyncAll(
	_ context.Context, _ syncpkg.ProgressFunc,
) syncpkg.SyncStats {
	s.syncAllCalls++
	return syncpkg.SyncStats{}
}

func (s *countingArtifactWatchSyncer) FlushSignals() {
	s.flushCalls++
}

func openWatchTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })
	return database
}

func seedStarredSession(t *testing.T, database *db.DB, id string) {
	t.Helper()
	require.NoError(t, database.UpsertSession(db.Session{
		ID:               id,
		Project:          "alpha",
		Machine:          "local",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-06-14T01:02:03Z",
	}))
	require.NoError(t, database.ReplaceSessionMessages(id, []db.Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
	}))
	starred, err := database.StarSession(id)
	require.NoError(t, err, "StarSession")
	require.True(t, starred, "session should be newly starred")
}

func TestArtifactFolderPusherPublishesBaselineOnFirstSuccessfulPush(t *testing.T) {
	dataDir := t.TempDir()
	target := t.TempDir()
	database := openWatchTestDB(t)
	seedStarredSession(t, database, "sess-1")

	pusher := &artifactFolderPusher{
		appCfg:   config.Config{DataDir: dataDir},
		database: database,
		target:   target,
		origin:   "laptop-a1b2c3",
		baseline: true,
	}
	require.NoError(t, pusher.push(context.Background(), reasonStartup))
	assert.False(t, pusher.baseline,
		"baseline must be published at most once after a successful push")

	events, err := filepath.Glob(
		filepath.Join(target, "laptop-a1b2c3", "meta", "*"),
	)
	require.NoError(t, err)
	assert.NotEmpty(t, events,
		"--init --watch must publish baseline metadata events to the target")
}

func TestArtifactFolderPusherRetainsBaselineAfterFailedPush(t *testing.T) {
	dataDir := t.TempDir()
	// A regular file as the folder target makes the exchange fail.
	target := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o600))
	database := openWatchTestDB(t)
	seedStarredSession(t, database, "sess-1")

	pusher := &artifactFolderPusher{
		appCfg:   config.Config{DataDir: dataDir},
		database: database,
		target:   target,
		origin:   "laptop-a1b2c3",
		baseline: true,
	}
	require.Error(t, pusher.push(context.Background(), reasonStartup))
	assert.True(t, pusher.baseline,
		"a failed push must keep the baseline pending for the next retry")
}

func TestArtifactFolderPusherPlumbsInsecurePeerOptIn(t *testing.T) {
	target, requests := insecureArtifactPeerTarget(t)
	dataDir := t.TempDir()
	database := openWatchTestDB(t)
	pusher := &artifactFolderPusher{
		appCfg:        config.Config{DataDir: dataDir},
		database:      database,
		target:        target,
		origin:        "desk-a1b2c3",
		allowInsecure: true,
	}

	require.NoError(t, pusher.push(context.Background(), reasonStartup))
	assert.Positive(t, requests.Load(),
		"watch sync must reach an explicitly allowed plaintext peer")
}

func TestArtifactFolderPusherRunsFullDiscoveryForIntervalAndShutdown(t *testing.T) {
	dataDir := t.TempDir()
	target := t.TempDir()
	database := openWatchTestDB(t)
	syncer := &countingArtifactWatchSyncer{}
	pusher := &artifactFolderPusher{
		appCfg:   config.Config{DataDir: dataDir},
		database: database,
		engine:   syncer,
		target:   target,
		origin:   "desk-a1b2c3",
	}

	for _, reason := range []pushReason{reasonStartup, reasonChange} {
		require.NoError(t, pusher.push(context.Background(), reason))
	}
	assert.Zero(t, syncer.syncAllCalls,
		"startup and watcher-driven pushes already synchronized local files")
	assert.Equal(t, 2, syncer.flushCalls,
		"every export must flush pending signal recomputes")

	require.NoError(t, pusher.push(context.Background(), reasonShutdown))
	assert.Equal(t, 1, syncer.syncAllCalls,
		"shutdown must discover changes still pending in the watcher batch")

	require.NoError(t, pusher.push(context.Background(), reasonInterval))
	assert.Equal(t, 2, syncer.syncAllCalls,
		"the periodic floor must discover changes from unwatched roots")
	assert.Equal(t, 4, syncer.flushCalls)
}

func TestArtifactFolderPusherShutdownDiscoversPendingWatcherChange(t *testing.T) {
	claudeDir := t.TempDir()
	projectDir := filepath.Join(claudeDir, "-Users-alice-work")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	database := openWatchTestDB(t)
	appCfg := config.Config{
		DataDir: t.TempDir(),
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir},
		},
	}
	engine := newArtifactWatchEngine(database, appCfg)
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(context.Background(), nil).Synced)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-01-01T00:00:00Z", "pending", "/Users/alice/work/project").
		AddClaudeAssistant("2026-01-01T00:00:01Z", "ok").
		String()
	require.NoError(t, os.WriteFile(
		filepath.Join(projectDir, "pending-session.jsonl"), []byte(content), 0o644,
	))

	target := t.TempDir()
	pusher := &artifactFolderPusher{
		appCfg:   appCfg,
		database: database,
		engine:   engine,
		target:   target,
		origin:   "desk-a1b2c3",
	}
	require.NoError(t, pusher.push(context.Background(), reasonShutdown))

	session, err := database.GetSession(context.Background(), "pending-session")
	require.NoError(t, err)
	require.NotNil(t, session,
		"shutdown flush must ingest a file whose watcher event is still pending")
	assert.Equal(t, "pending", *session.FirstMessage)
	pending, err := database.PendingArtifactExports(context.Background(), 1)
	require.NoError(t, err)
	assert.Empty(t, pending,
		"shutdown flush must publish every newly discovered session")

	checkpoints, err := filepath.Glob(filepath.Join(
		target, "desk-a1b2c3", string(artifact.KindCheckpoints), "cp-*.json",
	))
	require.NoError(t, err)
	require.Len(t, checkpoints, 1)
	latest, err := os.ReadFile(checkpoints[0])
	require.NoError(t, err)
	var checkpoint struct {
		Sessions map[string]string `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(latest, &checkpoint))
	assert.Contains(t, checkpoint.Sessions, "desk-a1b2c3~pending-session",
		"shutdown flush must make the discovered session reachable from the final checkpoint")
}

func TestArtifactWatchEngineHonorsConfiguredCwdPrefixes(t *testing.T) {
	claudeDir := t.TempDir()
	projectDir := filepath.Join(claudeDir, "-Users-alice-work")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	writeSession := func(name, cwd, prompt string) {
		content := testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-01-01T00:00:00Z", prompt, cwd).
			AddClaudeAssistant("2026-01-01T00:00:01Z", "ok").
			String()
		require.NoError(t, os.WriteFile(
			filepath.Join(projectDir, name+".jsonl"), []byte(content), 0o644,
		))
	}
	writeSession("allowed-session", "/Users/alice/work/project", "allowed")
	writeSession("blocked-session", "/Users/alice/personal/project", "blocked")

	database := openWatchTestDB(t)
	engine := newArtifactWatchEngine(database, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir},
		},
		SyncIncludeCwdPrefixes: []string{"/Users/alice/work"},
	})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Synced)

	allowed, err := database.GetSession(context.Background(), "allowed-session")
	require.NoError(t, err)
	require.NotNil(t, allowed)
	assert.Equal(t, "allowed", *allowed.FirstMessage)
	blocked, err := database.GetSession(context.Background(), "blocked-session")
	require.NoError(t, err)
	assert.Nil(t, blocked,
		"watch mode must not ingest sessions outside sync_include_cwd_prefixes")
}

func TestResolveArtifactOriginPromotesDBOriginIntoConfig(t *testing.T) {
	dir := t.TempDir()
	appCfg := config.Config{DataDir: dir}
	database := openWatchTestDB(t)
	require.NoError(t, artifact.AdoptOrigin(database, "laptop-a1b2c3"),
		"seed DB-only origin")

	origin, err := resolveArtifactOrigin(appCfg, database)
	require.NoError(t, err)
	assert.Equal(t, "laptop-a1b2c3", origin,
		"a DB-only origin must be reused, not replaced by a generated one")

	data, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(data), `artifact_origin_id = "laptop-a1b2c3"`,
		"the DB origin must be promoted into config so serve adopts the same origin")
}

func TestResolveArtifactOriginConfigWinsOverDB(t *testing.T) {
	dir := t.TempDir()
	appCfg := config.Config{DataDir: dir, ArtifactOriginID: "desktop-d4e5f6"}
	database := openWatchTestDB(t)
	require.NoError(t, artifact.AdoptOrigin(database, "laptop-a1b2c3"),
		"seed DB origin")

	origin, err := resolveArtifactOrigin(appCfg, database)
	require.NoError(t, err)
	assert.Equal(t, "desktop-d4e5f6", origin)

	stored, err := artifact.StoredOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, "desktop-d4e5f6", stored,
		"the authoritative config origin must replace a divergent DB origin")
}

func TestResolveArtifactOriginGeneratesWhenAbsentEverywhere(t *testing.T) {
	dir := t.TempDir()
	appCfg := config.Config{DataDir: dir}
	database := openWatchTestDB(t)

	origin, err := resolveArtifactOrigin(appCfg, database)
	require.NoError(t, err)
	assert.Regexp(t, `^[a-z0-9]+(?:-[a-z0-9]+)*-[0-9a-f]{6}$`, origin)

	stored, err := artifact.StoredOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, origin, stored,
		"a directly generated CLI origin must be available to DB-only consumers")
}
