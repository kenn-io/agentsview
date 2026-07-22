package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestReconcileWatchRootsWindsurf300MembersUsesOneExactBoundedScan(t *testing.T) {
	const members = 300
	root := filepath.Join(t.TempDir(), "Windsurf", "User")
	workspaceDir := filepath.Join(root, "workspaceStorage", "workspace-hash")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	manifest, err := json.Marshal(map[string]string{
		"folder": "file:///work/demo", "padding": strings.Repeat("m", 1<<20),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "workspace.json"), manifest, 0o600,
	))
	dbPath := filepath.Join(workspaceDir, "state.vscdb")
	type bubble struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type tab struct {
		ID      string   `json:"tabId"`
		Bubbles []bubble `json:"bubbles"`
	}
	payload := struct {
		Tabs []tab `json:"tabs"`
	}{Tabs: make([]tab, members)}
	for i := range members {
		payload.Tabs[i] = tab{
			ID: fmt.Sprintf("session-%03d", i),
			Bubbles: []bubble{
				{Type: "user", Text: strings.Repeat("question ", 2048)},
				{Type: "assistant", Text: "bounded answer"},
			},
		}
	}
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	writeSyncWindsurfStateDB(t, dbPath, string(encoded))
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentWindsurf: {root}},
		Machine:   "local",
	})
	engine.workerCountOverride = 2
	t.Cleanup(engine.Close)

	overlapTarget := min(4, engine.workerCount())
	require.Equal(t, 2, overlapTarget)
	probe := newRetainedOverlapProbe(parser.AgentWindsurf, overlapTarget)
	ctx := parser.WithReconciliationRetainedMemberObserver(t.Context(), probe.observe)
	done := make(chan error, 1)
	go func() {
		done <- engine.ReconcileWatchRoots(ctx, []string{root}, false)
	}()
	probe.waitAndRelease(t)
	require.NoError(t, <-done)

	result := engine.LastReconciliationResult()
	assert.True(t, result.Complete)
	assert.Equal(t, 1, result.Metrics.SharedContainerScans,
		"exact rehydration must not rescan the container per member")
	assert.Equal(t, reconciliationPageSize, result.Metrics.MaxSpoolPageRows)
	assert.Positive(t, result.Metrics.MaxProviderRetainedBytes)
	assert.GreaterOrEqual(t, probe.maxActive.Load(), int32(overlapTarget))
	assert.GreaterOrEqual(t, result.Metrics.MaxProviderRetainedBytes, probe.maxBytes.Load(),
		"aggregate metric must include every concurrently retained worker member")
	assert.GreaterOrEqual(t, result.Metrics.MaxProviderRetainedBytes, int64(len(manifest)),
		"workspace manifest must be charged for its discovery lifetime")
	assert.Less(t, result.Metrics.MaxProviderRetainedBytes, int64(len(encoded))/2,
		"concurrent provider retention must stay bounded by members, not the container")
	stored, getErr := database.GetSession(t.Context(), "windsurf:session-299")
	require.NoError(t, getErr)
	require.NotNil(t, stored)
}

func TestReconcileWatchRootsWindsurfTombstonesDeletedVirtualMember(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Windsurf", "User")
	workspaceDir := filepath.Join(root, "workspaceStorage", "workspace-hash")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "workspace.json"),
		[]byte(`{"folder":"file:///work/demo"}`), 0o600,
	))
	dbPath := filepath.Join(workspaceDir, "state.vscdb")
	writeSyncWindsurfStateDB(t, dbPath, windsurfTabsPayload(t,
		"surviving-member", "deleted-member",
	))
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentWindsurf: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	updateSyncWindsurfStateDB(t, dbPath, windsurfTabsPayload(t, "surviving-member"))

	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{root}, false))

	active, err := database.GetSession(t.Context(), "windsurf:deleted-member")
	require.NoError(t, err)
	assert.Nil(t, active)
	archived, err := database.GetSessionFull(t.Context(), "windsurf:deleted-member")
	require.NoError(t, err)
	require.NotNil(t, archived)
	require.NotNil(t, archived.DeletionCause)
	assert.Equal(t, "source_missing", *archived.DeletionCause)
	surviving, err := database.GetSession(t.Context(), "windsurf:surviving-member")
	require.NoError(t, err)
	assert.NotNil(t, surviving)
}

func TestSyncPathsWindsurfTombstonesRemovedVirtualMember(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Windsurf", "User")
	workspaceDir := filepath.Join(root, "workspaceStorage", "workspace-hash")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "workspace.json"),
		[]byte(`{"folder":"file:///work/demo"}`), 0o600,
	))
	dbPath := filepath.Join(workspaceDir, "state.vscdb")
	writeSyncWindsurfStateDB(t, dbPath, windsurfTabsPayload(t,
		"surviving-member", "removed-member",
	))
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentWindsurf: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)

	// The container file still exists; only one member row is removed,
	// exactly what the watcher sees after Windsurf deletes a conversation.
	updateSyncWindsurfStateDB(t, dbPath, windsurfTabsPayload(t, "surviving-member"))

	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))

	active, err := database.GetSession(t.Context(), "windsurf:removed-member")
	require.NoError(t, err)
	assert.Nil(t, active, "removed member must not stay active")
	archived, err := database.GetSessionFull(t.Context(), "windsurf:removed-member")
	require.NoError(t, err)
	require.NotNil(t, archived,
		"removed member must be preserved in the archive, not hard-deleted")
	require.NotNil(t, archived.DeletionCause)
	assert.Equal(t, "source_missing", *archived.DeletionCause)
	surviving, err := database.GetSession(t.Context(), "windsurf:surviving-member")
	require.NoError(t, err)
	assert.NotNil(t, surviving)
}

func windsurfTabsPayload(t *testing.T, sessionIDs ...string) string {
	t.Helper()
	type bubble struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type tab struct {
		ID      string   `json:"tabId"`
		Bubbles []bubble `json:"bubbles"`
	}
	payload := struct {
		Tabs []tab `json:"tabs"`
	}{Tabs: make([]tab, len(sessionIDs))}
	for i, sessionID := range sessionIDs {
		payload.Tabs[i] = tab{
			ID: sessionID,
			Bubbles: []bubble{
				{Type: "user", Text: "question for " + sessionID},
				{Type: "assistant", Text: "answer for " + sessionID},
			},
		}
	}
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	return string(encoded)
}

type retainedOverlapProbe struct {
	provider  parser.AgentType
	target    int
	arrived   chan struct{}
	release   chan struct{}
	active    atomic.Int32
	liveBytes atomic.Int64
	maxActive atomic.Int32
	maxBytes  atomic.Int64
}

func newRetainedOverlapProbe(provider parser.AgentType, target int) *retainedOverlapProbe {
	return &retainedOverlapProbe{
		provider: provider, target: target,
		arrived: make(chan struct{}, target), release: make(chan struct{}),
	}
}

func (probe *retainedOverlapProbe) observe(provider parser.AgentType, retained int64) {
	if provider != probe.provider || retained <= 0 {
		return
	}
	active := probe.active.Add(1)
	liveBytes := probe.liveBytes.Add(retained)
	atomicMaxInt32(&probe.maxActive, active)
	atomicMaxInt64(&probe.maxBytes, liveBytes)
	select {
	case probe.arrived <- struct{}{}:
	case <-probe.release:
	}
	<-probe.release
	probe.liveBytes.Add(-retained)
	probe.active.Add(-1)
}

func (probe *retainedOverlapProbe) waitAndRelease(t *testing.T) {
	t.Helper()
	defer func() {
		select {
		case <-probe.release:
		default:
			close(probe.release)
		}
	}()
	for range probe.target {
		select {
		case <-probe.arrived:
		case <-time.After(30 * time.Second):
			t.Fatal("timed out waiting for concurrent retained members")
		}
	}
	close(probe.release)
}

func atomicMaxInt32(value *atomic.Int32, candidate int32) {
	for current := value.Load(); candidate > current; current = value.Load() {
		if value.CompareAndSwap(current, candidate) {
			return
		}
	}
}

func atomicMaxInt64(value *atomic.Int64, candidate int64) {
	for current := value.Load(); candidate > current; current = value.Load() {
		if value.CompareAndSwap(current, candidate) {
			return
		}
	}
}

func TestReconcileWatchRootsWindsurfDiskIndexFailuresStayIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(context.Context, error) context.Context
	}{
		{name: "query", inject: parser.WithDiscoveryDiskMapQueryError},
		{name: "cleanup", inject: parser.WithDiscoveryDiskMapCleanupError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "Windsurf", "User")
			workspaceDir := filepath.Join(root, "workspaceStorage", "workspace-hash")
			require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
			dbPath := filepath.Join(workspaceDir, "state.vscdb")
			writeSyncWindsurfStateDB(t, dbPath, windsurfSyncPayload("fault-session", "reply"))
			database := dbtest.OpenTestDB(t)
			engine := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentWindsurf: {root}},
			})
			defer engine.Close()
			injected := errors.New("injected discovery index " + tc.name)
			ctx := tc.inject(t.Context(), injected)

			err := engine.ReconcileWatchRoots(ctx, []string{root}, false)

			require.ErrorIs(t, err, injected)
			result := engine.LastReconciliationResult()
			assert.False(t, result.Complete)
			assert.True(t, result.Aborted)
			assert.Positive(t, result.ProviderFailures)
		})
	}
}

func TestSourceMtimeWindsurfUsesProviderFingerprint(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Windsurf", "User")
	workspaceDir := filepath.Join(root, "workspaceStorage", "workspace-hash")
	manifestPath := filepath.Join(workspaceDir, "workspace.json")
	dbPath := filepath.Join(workspaceDir, "state.vscdb")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	require.NoError(t, os.WriteFile(manifestPath, []byte(`{"folder":"file:///work/demo"}`), 0o644))
	writeSyncWindsurfStateDB(t, dbPath, `{
		"version": 1,
		"sessionId": "mtime-session",
		"requests": [{
			"requestId": "request-1",
			"message": {"text": "Question"},
			"response": [{"value": "Answer"}],
			"timestamp": 1710000000000
		}]
	}`)
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentWindsurf: {root},
		},
		Machine: "devbox",
	})
	defer engine.Close()

	stats := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Synced)
	virtualPath := dbPath + "#mtime-session"
	assert.Equal(t, virtualPath, engine.FindSourceFile("windsurf:mtime-session"))
	before := engine.SourceMtime("windsurf:mtime-session")
	require.NotZero(t, before)

	future := time.Unix(0, before).Add(2 * time.Second)
	require.NoError(t, os.Chtimes(manifestPath, future, future))

	after := engine.SourceMtime("windsurf:mtime-session")
	assert.Greater(t, after, before)
}

func TestProcessFileWindsurfSameMtimeHashChangeReparses(t *testing.T) {
	for _, tt := range []struct {
		name      string
		seedCache bool
		freshSync bool
	}{
		{name: "skip cache", seedCache: true},
		{name: "db freshness", freshSync: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "Windsurf", "User")
			workspaceDir := filepath.Join(root, "workspaceStorage", "workspace-hash")
			manifestPath := filepath.Join(workspaceDir, "workspace.json")
			dbPath := filepath.Join(workspaceDir, "state.vscdb")
			require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
			require.NoError(t, os.WriteFile(manifestPath, []byte(`{"folder":"file:///work/demo"}`), 0o644))
			writeSyncWindsurfStateDB(t, dbPath, windsurfSyncPayload("hash-session", "Alpha reply"))
			virtualPath := dbPath + "#hash-session"
			database := dbtest.OpenTestDB(t)
			engine := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentWindsurf: {root},
				},
				Machine: "devbox",
			})
			defer engine.Close()

			initialMtime, initialHash := syncInitialWindsurfSession(
				t, engine, "hash-session",
			)

			infoBefore, err := os.Stat(dbPath)
			require.NoError(t, err)
			updateSyncWindsurfStateDB(t, dbPath, windsurfSyncPayload("hash-session", "Bravo reply"))
			initialTime := time.Unix(0, initialMtime)
			require.NoError(t, os.Chtimes(dbPath, initialTime, initialTime))
			infoAfter, err := os.Stat(dbPath)
			require.NoError(t, err)
			require.Equal(t, infoBefore.Size(), infoAfter.Size(),
				"test must keep size stable so hash is the only freshness signal")

			if tt.seedCache {
				engine.cacheSkip(
					providerProcessCacheKeyWithHash(
						virtualPath,
						parser.AgentWindsurf,
						parser.SourceFingerprint{Hash: initialHash},
					),
					initialMtime,
				)
			}
			if tt.freshSync {
				engine.Close()
				engine = NewEngine(database, EngineConfig{
					AgentDirs: map[parser.AgentType][]string{
						parser.AgentWindsurf: {root},
					},
					Machine: "devbox",
				})
				defer engine.Close()
			}

			second := engine.processFile(context.Background(), parser.DiscoveredFile{
				Path:  virtualPath,
				Agent: parser.AgentWindsurf,
			})
			require.NoError(t, second.err)
			assert.False(t, second.skip)
			require.Len(t, second.results, 1)
			require.Len(t, second.results[0].Messages, 2)
			assert.Equal(t, "Bravo reply", second.results[0].Messages[1].Content)
			assert.NotEqual(t, initialHash, second.results[0].Session.File.Hash)
		})
	}
}

func writeSyncWindsurfStateDB(t *testing.T, dbPath, payload string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`)
	require.NoError(t, err)
	_, err = conn.Exec(
		`INSERT INTO ItemTable (key, value) VALUES (?, ?)`,
		"workbench.panel.aichat.view.aichat.chatdata",
		payload,
	)
	require.NoError(t, err)
}

func updateSyncWindsurfStateDB(t *testing.T, dbPath, payload string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(
		`UPDATE ItemTable SET value = ? WHERE key = ?`,
		payload,
		"workbench.panel.aichat.view.aichat.chatdata",
	)
	require.NoError(t, err)
}

func syncInitialWindsurfSession(
	t *testing.T,
	engine *Engine,
	sessionID string,
) (int64, string) {
	t.Helper()
	stats := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Synced)
	sess, err := engine.db.GetSessionFull(
		context.Background(), "windsurf:"+sessionID,
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.FileMtime)
	require.NotNil(t, sess.FileHash)
	require.NotZero(t, *sess.FileMtime)
	require.NotEmpty(t, *sess.FileHash)
	return *sess.FileMtime, *sess.FileHash
}

func windsurfSyncPayload(sessionID, assistant string) string {
	return `{
		"version": 1,
		"sessionId": "` + sessionID + `",
		"requests": [{
			"requestId": "request-1",
			"message": {"text": "Question"},
			"response": [{"value": "` + assistant + `"}],
			"timestamp": 1710000000000
		}]
	}`
}
