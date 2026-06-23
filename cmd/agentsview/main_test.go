package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/ssh"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestMustLoadConfig(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantHost      string
		wantPort      int
		wantPublicURL string
		wantProxyMode string
	}{
		{
			name:          "DefaultArgs",
			args:          []string{},
			wantHost:      "127.0.0.1",
			wantPort:      8080,
			wantPublicURL: "",
			wantProxyMode: "",
		},
		{
			name:          "ExplicitFlags",
			args:          []string{"--host", "0.0.0.0", "--port", "9090", "--public-url", "https://viewer.example.test", "--proxy", "caddy", "--proxy-bind-host", "10.0.60.2", "--public-port", "9443", "--no-browser"},
			wantHost:      "0.0.0.0",
			wantPort:      9090,
			wantPublicURL: "https://viewer.example.test:9443",
			wantProxyMode: "caddy",
		},
		{
			name:          "PartialFlags",
			args:          []string{"--port", "3000"},
			wantHost:      "127.0.0.1",
			wantPort:      3000,
			wantPublicURL: "",
			wantProxyMode: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testDataDir(t)
			cmd := newServeCommand()
			require.NoError(t, cmd.Flags().Parse(tt.args), "Parse")
			cfg := mustLoadConfig(cmd)

			assert.Equal(t, tt.wantHost, cfg.Host)
			assert.Equal(t, tt.wantPort, cfg.Port)
			assert.Equal(t, tt.wantPublicURL, cfg.PublicURL)
			assert.Equal(t, tt.wantProxyMode, cfg.Proxy.Mode)

			assert.NotEmpty(t, cfg.DataDir, "DataDir should be set")
			wantDBPath := filepath.Join(cfg.DataDir, "sessions.db")
			assert.Equal(t, wantDBPath, cfg.DBPath)
		})
	}
}

func TestPrepareServeRuntimeConfigPortZeroUsesAssignedPort(t *testing.T) {
	cfg := config.Config{
		Host: "127.0.0.1",
		Port: 0,
	}

	var err error
	out := captureStdout(t, func() {
		cfg, err = prepareServeRuntimeConfig(
			cfg,
			serveRuntimeOptions{
				Mode:          "serve",
				RequestedPort: 0,
			},
		)
	})
	require.NoError(t, err, "prepareServeRuntimeConfig")
	assert.NotZero(t, cfg.Port, "Port remained literal 0")
	assert.NotContains(t, out, "Port 0 in use",
		"unexpected literal port 0 fallback message")
	assert.Contains(t, out, "Using available port",
		"missing ephemeral port message")
}

func TestSetupLogFile(t *testing.T) {
	dir := t.TempDir()
	// Register after TempDir so LIFO cleanup closes the log file before
	// TempDir removes the directory. On Windows, open files can't be deleted.
	restoreTestLogOutput(t)

	setupLogFile(dir)

	// Log something and verify it reaches the file.
	log.Print("test-log-message")

	logPath := filepath.Join(dir, "debug.log")
	data, err := os.ReadFile(logPath)
	require.NoError(t, err, "reading log file")
	assert.Contains(t, string(data), "test-log-message",
		"log file missing message")
}

func TestSetupLogFileOpenFailure(t *testing.T) {
	// Capture log output to verify warning is emitted.
	buf := captureLogOutput(t)

	// Pass a path that can't be opened (dir doesn't exist
	// and we use a file as the "dir").
	tmpFile := filepath.Join(t.TempDir(), "notadir")
	writeTestFile(t, tmpFile, []byte("x"))

	setupLogFile(tmpFile)

	assert.Contains(t, buf.String(), "cannot open log file",
		"expected warning about log file")
}

func TestTruncateLogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// Write a file larger than the limit.
	big := bytes.Repeat([]byte("x"), 1024)
	writeTestFile(t, path, big)

	// Truncate with limit smaller than file size.
	truncateLogFile(path, 512)

	info, err := os.Stat(path)
	require.NoError(t, err, "stat after truncate")
	assert.Equal(t, int64(0), info.Size())
}

func TestTruncateLogFileUnderLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	content := []byte("small log content")
	writeTestFile(t, path, content)

	// File is under limit: should not be truncated.
	truncateLogFile(path, 1024)

	data, err := os.ReadFile(path)
	require.NoError(t, err, "read after truncate")
	assert.Equal(t, string(content), string(data), "content changed")
}

func TestTruncateLogFileMissing(t *testing.T) {
	// Non-existent file: should not panic.
	missing := filepath.Join(t.TempDir(), "missing", "log.txt")
	truncateLogFile(missing, 1024)
}

func TestTruncateLogFileSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.log")
	link := filepath.Join(dir, "link.log")

	// Write a target file larger than the limit.
	big := bytes.Repeat([]byte("x"), 1024)
	writeTestFile(t, target, big)
	requireSymlinkOrSkip(t, target, link)

	// Truncate via symlink: should be a no-op.
	truncateLogFile(link, 512)

	data, err := os.ReadFile(target)
	require.NoError(t, err, "read target")
	assert.Len(t, data, 1024, "symlink target was truncated")
}

type fakeUnwatchedPollSyncer struct {
	roots     []string
	since     time.Time
	calls     int
	callRoots [][]string
	callSince []time.Time
}

func (f *fakeUnwatchedPollSyncer) SyncRootsSince(
	ctx context.Context, roots []string, since time.Time,
	onProgress agentsync.ProgressFunc,
) agentsync.SyncStats {
	f.calls++
	f.roots = append([]string(nil), roots...)
	f.since = since
	f.callRoots = append(f.callRoots, append([]string(nil), roots...))
	f.callSince = append(f.callSince, since)
	return agentsync.SyncStats{}
}

func TestPollUnwatchedRootsOnceUsesScopedFullSync(t *testing.T) {
	fake := &fakeUnwatchedPollSyncer{}
	roots := []string{"/tmp/claude", "/tmp/codex"}

	pollUnwatchedRootsOnce(fake, roots)
	pollUnwatchedRootsOnce(fake, roots)

	require.Equal(t, 2, fake.calls)
	assert.Equal(t, roots, fake.callRoots[0])
	assert.True(t, fake.callSince[0].IsZero(), "first poll cutoff = %v", fake.callSince[0])
	assert.Equal(t, roots, fake.callRoots[1])
	assert.True(t, fake.callSince[1].IsZero(), "second poll cutoff = %v", fake.callSince[1])
}

func TestCollectWatchRootsPreservesDirsSharingWatchRoot(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "codex-state")
	require.NoError(t, os.Mkdir(parent, 0o755), "mkdir parent")

	sessionsDir := filepath.Join(parent, "sessions")
	archivedDir := filepath.Join(parent, "archived_sessions")
	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {sessionsDir, archivedDir},
		},
	}

	roots, unwatchedDirs := collectWatchRoots(cfg)

	require.Empty(t, unwatchedDirs, "unwatched dirs before watcher setup")
	require.Len(t, roots, 1, "shared watch root should be represented once")
	assert.Equal(t, parent, roots[0].root)
	assert.ElementsMatch(t, []string{sessionsDir, archivedDir}, roots[0].dirs)
}

// fakeEmitter records Emit calls; safe for concurrent use.
type fakeEmitter struct {
	count atomic.Int64
}

func (f *fakeEmitter) Emit(_ string) { f.count.Add(1) }

func TestStartRemoteHostSync_EmitsAfterSuccess(t *testing.T) {
	em := &fakeEmitter{}
	syncFn := func() (int, error) { return 3, nil }

	done := make(chan struct{})
	exited := make(chan struct{})
	interval := 10 * time.Millisecond
	go func() {
		runRemoteHostSyncLoop(context.Background(), "test-host", interval, syncFn, em, nil, done)
		close(exited)
	}()

	time.Sleep(3 * interval)
	close(done)
	<-exited

	assert.Positive(t, em.count.Load(), "emitter should have been called at least once")
}

func TestRemoteHostSyncFuncSerializesWithEngineExclusiveLock(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{},
		Machine:   "local",
	})

	remoteEntered := make(chan struct{})
	releaseRemote := make(chan struct{})
	syncFn := remoteHostSyncFunc(
		context.Background(),
		config.Config{},
		database,
		engine,
		config.RemoteHost{Host: "test-host"},
		func(context.Context, *ssh.RemoteSync) (ssh.SyncStats, error) {
			close(remoteEntered)
			<-releaseRemote
			return ssh.SyncStats{}, nil
		},
	)

	syncErr := make(chan error, 1)
	go func() {
		_, err := syncFn()
		syncErr <- err
	}()

	select {
	case <-remoteEntered:
	case <-time.After(time.Second):
		require.FailNow(t, "remote sync did not enter")
	}

	exclusiveEntered := make(chan struct{})
	exclusiveErr := make(chan error, 1)
	go func() {
		exclusiveErr <- engine.RunExclusive(func() error {
			close(exclusiveEntered)
			return nil
		})
	}()

	select {
	case <-exclusiveEntered:
		assert.Fail(t, "exclusive operation overlapped scheduled remote sync")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseRemote)

	select {
	case err := <-syncErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		require.FailNow(t, "scheduled remote sync did not finish")
	}
	select {
	case err := <-exclusiveErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		require.FailNow(t, "exclusive operation did not finish")
	}
}

func TestRemoteHostSyncFuncUsesCallerContext(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{},
		Machine:   "local",
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	syncFn := remoteHostSyncFunc(
		ctx,
		config.Config{},
		database,
		engine,
		config.RemoteHost{Host: "test-host"},
		func(runCtx context.Context, _ *ssh.RemoteSync) (ssh.SyncStats, error) {
			return ssh.SyncStats{}, runCtx.Err()
		},
	)

	_, err = syncFn()

	require.ErrorIs(t, err, context.Canceled)
}

func TestRemoteHostSyncFuncForcesFullWhenDatabaseNeedsResync(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = raw.Exec("PRAGMA user_version = 0")
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	database, err = db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	require.True(t, database.NeedsResync())
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{},
		Machine:   "local",
	})

	var gotFull bool
	syncFn := remoteHostSyncFunc(
		context.Background(),
		config.Config{},
		database,
		engine,
		config.RemoteHost{Host: "test-host"},
		func(_ context.Context, rs *ssh.RemoteSync) (ssh.SyncStats, error) {
			gotFull = rs.Full
			return ssh.SyncStats{}, nil
		},
	)

	_, err = syncFn()

	require.NoError(t, err)
	assert.True(t, gotFull, "scheduled remote sync should force full when DB needs resync")
}

func TestStartRemoteHostSync_TracksRemoteWorkForIdleReaper(t *testing.T) {
	idleFired := make(chan struct{})
	idleTracker := server.NewIdleTracker(20*time.Millisecond, func() {
		close(idleFired)
	})
	ctx := t.Context()

	syncEntered := make(chan struct{}, 1)
	releaseSync := make(chan struct{})
	syncFn := func() (int, error) {
		select {
		case syncEntered <- struct{}{}:
		default:
		}
		<-releaseSync
		return 1, nil
	}

	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		runRemoteHostSyncLoop(ctx, "test-host", time.Millisecond, syncFn, nil, idleTracker, done)
		close(exited)
	}()

	select {
	case <-syncEntered:
	case <-time.After(time.Second):
		require.FailNow(t, "remote sync did not enter")
	}
	go idleTracker.Run(ctx)

	select {
	case <-idleFired:
		require.FailNow(t, "idle tracker fired while remote sync was active")
	case <-time.After(80 * time.Millisecond):
	}

	close(releaseSync)
	close(done)
	select {
	case <-exited:
	case <-time.After(time.Second):
		require.FailNow(t, "remote sync loop did not exit")
	}
}

func TestStartRemoteHostSync_ExitsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	syncCalled := make(chan struct{}, 1)
	go func() {
		runRemoteHostSyncLoop(
			ctx,
			"test-host",
			time.Hour,
			func() (int, error) {
				syncCalled <- struct{}{}
				return 0, nil
			},
			nil,
			nil,
			nil,
		)
		close(exited)
	}()

	cancel()

	select {
	case <-exited:
	case <-time.After(time.Second):
		require.FailNow(t, "remote sync loop did not exit after context cancel")
	}
	select {
	case <-syncCalled:
		require.FailNow(t, "sync ran before context cancel")
	default:
	}
}

type scopedEmitter struct {
	scopes chan string
}

func (e *scopedEmitter) Emit(scope string) {
	select {
	case e.scopes <- scope:
	default:
	}
}

func TestStartRemoteHostSync_EmitsSessionsScopeAfterSuccess(t *testing.T) {
	em := &scopedEmitter{scopes: make(chan string, 1)}
	syncFn := func() (int, error) { return 3, nil }

	done := make(chan struct{})
	exited := make(chan struct{})
	interval := 10 * time.Millisecond
	go func() {
		runRemoteHostSyncLoop(context.Background(), "test-host", interval, syncFn, em, nil, done)
		close(exited)
	}()

	select {
	case scope := <-em.scopes:
		assert.Equal(t, "sessions", scope)
	case <-time.After(3 * interval):
		require.FailNow(t, "timed out waiting for remote sync event")
	}
	close(done)
	<-exited
}

func TestStartRemoteHostSync_NoEmitOnZeroSynced(t *testing.T) {
	em := &fakeEmitter{}
	syncFn := func() (int, error) { return 0, nil }

	done := make(chan struct{})
	exited := make(chan struct{})
	interval := 10 * time.Millisecond
	go func() {
		runRemoteHostSyncLoop(context.Background(), "test-host", interval, syncFn, em, nil, done)
		close(exited)
	}()

	time.Sleep(3 * interval)
	close(done)
	<-exited

	assert.Zero(t, em.count.Load(), "emitter should not fire when no sessions synced")
}

func TestStartRemoteHostSync_NoEmitOnError(t *testing.T) {
	em := &fakeEmitter{}
	syncFn := func() (int, error) { return 0, errors.New("ssh failure") }

	done := make(chan struct{})
	exited := make(chan struct{})
	interval := 10 * time.Millisecond
	go func() {
		runRemoteHostSyncLoop(context.Background(), "test-host", interval, syncFn, em, nil, done)
		close(exited)
	}()

	time.Sleep(3 * interval)
	close(done)
	<-exited

	assert.Zero(t, em.count.Load(), "emitter should not fire when sync fails")
}

func TestStartRemoteHostSync_NilEmitterSafe(t *testing.T) {
	syncFn := func() (int, error) { return 1, nil }

	done := make(chan struct{})
	exited := make(chan struct{})
	interval := 10 * time.Millisecond
	go func() {
		runRemoteHostSyncLoop(context.Background(), "test-host", interval, syncFn, nil, nil, done)
		close(exited)
	}()

	time.Sleep(2 * interval)
	close(done)
	<-exited
}

func TestCollectWatchRootsHermesSessionsWatchesStateDBParent(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.Mkdir(sessionsDir, 0o755), "mkdir sessions")

	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
	}

	roots, unwatchedDirs := collectWatchRoots(cfg)

	require.Empty(t, unwatchedDirs, "unwatched dirs before watcher setup")
	require.Len(t, roots, 2)
	assert.Equal(t, root, roots[0].root)
	assert.True(t, roots[0].shallow)
	assert.Equal(t, []string{sessionsDir}, roots[0].dirs)
	assert.Equal(t, sessionsDir, roots[1].root)
	assert.False(t, roots[1].shallow)
	assert.Equal(t, []string{sessionsDir}, roots[1].dirs)
}

func TestResyncCoversSignals(t *testing.T) {
	tests := []struct {
		name     string
		stats    agentsync.SyncStats
		fellBack bool
		want     bool
	}{
		{
			name:  "clean resync no orphans covers signals",
			stats: agentsync.SyncStats{Synced: 5},
			want:  true,
		},
		{
			name: "fell back to incremental sync needs backfill",
			stats: agentsync.SyncStats{
				Synced: 2, Aborted: true,
			},
			fellBack: true,
			want:     false,
		},
		{
			name: "orphans copied need backfill",
			stats: agentsync.SyncStats{
				Synced: 5, OrphanedCopied: 3,
			},
			want: false,
		},
		{
			name: "orphans copied even with fallback false",
			stats: agentsync.SyncStats{
				Synced: 0, OrphanedCopied: 1,
			},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resyncCoversSignals(tc.stats, tc.fellBack)
			assert.Equal(t, tc.want, got)
		})
	}
}

type fakeSignalsBackfillMarker struct {
	calls int
	err   error
}

func (f *fakeSignalsBackfillMarker) MarkSignalsBackfillDone() error {
	f.calls++
	return f.err
}

func TestFinishInitialResyncMarksCoveredSignals(t *testing.T) {
	marker := &fakeSignalsBackfillMarker{}

	finishInitialResync(marker, true)

	assert.Equal(t, 1, marker.calls)
}

func TestFinishInitialResyncSkipsMarkerWhenSignalsNeedBackfill(t *testing.T) {
	marker := &fakeSignalsBackfillMarker{}

	finishInitialResync(marker, false)

	assert.Equal(t, 0, marker.calls)
}

func TestFormatAnomalySummary(t *testing.T) {
	tests := []struct {
		name        string
		anomalies   agentsync.AnomalyStats
		wantEmpty   bool
		wantContain []string
		wantOmit    []string
	}{
		{
			name:      "clean run omits the section",
			anomalies: agentsync.AnomalyStats{},
			wantEmpty: true,
		},
		{
			name: "malformed lines only",
			anomalies: agentsync.AnomalyStats{
				MalformedLinesByAgent: map[string]int{
					"claude": 3, "codex": 1,
				},
				MalformedLinesTotal: 4,
			},
			wantContain: []string{
				"Parser anomalies (this run):",
				"malformed lines: 4 total",
				"claude: 3",
				"codex: 1",
			},
			wantOmit: []string{"sanitized fields"},
		},
		{
			name: "sanitize fixes only, zero categories omitted",
			anomalies: agentsync.AnomalyStats{
				Sanitize: agentsync.SanitizeStats{
					ControlCharsStripped: 2,
					ModelClamped:         1,
				},
			},
			wantContain: []string{
				"sanitized fields: 3 total",
				"control chars stripped: 2",
				"model clamped: 1",
			},
			wantOmit: []string{
				"malformed lines",
				"tokens clamped",
				"role coerced",
				"timestamps blanked",
			},
		},
		{
			name: "both sections present",
			anomalies: agentsync.AnomalyStats{
				MalformedLinesByAgent: map[string]int{"gemini": 7},
				MalformedLinesTotal:   7,
				Sanitize: agentsync.SanitizeStats{
					TokensClamped:     4,
					TimestampsBlanked: 1,
				},
			},
			wantContain: []string{
				"malformed lines: 7 total",
				"gemini: 7",
				"sanitized fields: 5 total",
				"tokens clamped: 4",
				"timestamps blanked: 1",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatAnomalySummary(tc.anomalies)
			if tc.wantEmpty {
				assert.Empty(t, got)
				return
			}
			for _, want := range tc.wantContain {
				assert.Contains(t, got, want)
			}
			for _, omit := range tc.wantOmit {
				assert.NotContains(t, got, omit)
			}
		})
	}
}

func TestPrintSyncSummaryAnomalySection(t *testing.T) {
	t.Run("clean run omits anomaly section", func(t *testing.T) {
		out := captureStdout(t, func() {
			printSyncSummary(agentsync.SyncStats{Synced: 3}, time.Now())
		})
		assert.Contains(t, out, "Sync complete: 3 sessions synced")
		assert.NotContains(t, out, "Parser anomalies")
	})

	t.Run("non-zero anomalies print the section", func(t *testing.T) {
		stats := agentsync.SyncStats{
			Synced: 2,
			Anomalies: agentsync.AnomalyStats{
				MalformedLinesByAgent: map[string]int{"claude": 5},
				MalformedLinesTotal:   5,
				Sanitize: agentsync.SanitizeStats{
					ControlCharsStripped: 2,
				},
			},
		}
		out := captureStdout(t, func() {
			printSyncSummary(stats, time.Now())
		})
		assert.Contains(t, out, "Parser anomalies (this run):")
		assert.Contains(t, out, "malformed lines: 5 total")
		assert.Contains(t, out, "claude: 5")
		assert.Contains(t, out, "control chars stripped: 2")
		// Anomaly section follows the one-line summary.
		idx := strings.Index(out, "Sync complete")
		anomalyIdx := strings.Index(out, "Parser anomalies")
		assert.Less(t, idx, anomalyIdx)
	})
}
