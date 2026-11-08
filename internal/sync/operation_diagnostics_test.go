package sync

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func captureOperationLogs(t *testing.T) *summaryLogBuffer {
	t.Helper()
	output, flags := log.Writer(), log.Flags()
	buffer := &summaryLogBuffer{}
	log.SetOutput(buffer)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(output)
		log.SetFlags(flags)
	})
	return buffer
}

func operationLogRecords(t *testing.T, output string) []map[string]string {
	t.Helper()
	var records []map[string]string
	for line := range strings.SplitSeq(output, "\n") {
		if !strings.HasPrefix(line, "sync operation: ") {
			continue
		}
		record := make(map[string]string)
		for field := range strings.FieldsSeq(strings.TrimPrefix(line, "sync operation: ")) {
			key, value, ok := strings.Cut(field, "=")
			require.True(t, ok, "operation fields must be key=value: %s", field)
			_, duplicate := record[key]
			require.False(t, duplicate, "duplicate operation field: %s", key)
			record[key] = value
		}
		require.Equal(t, strconv.Itoa(os.Getpid()), record["pid"])
		delete(record, "pid")
		for _, key := range []string{"elapsed", "lock_wait", "running"} {
			value, ok := record[key]
			if !ok {
				continue
			}
			duration, err := time.ParseDuration(value)
			require.NoError(t, err)
			require.GreaterOrEqual(t, duration, time.Duration(0))
			record[key] = "duration"
		}
		records = append(records, record)
	}
	return records
}

func TestRecoveryDiagnosticsAccumulateAcrossPages(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sources     int
		wantSources string
		wantPages   string
	}{
		{name: "small", sources: 1, wantSources: "1", wantPages: "1"},
		{name: "multiple pages", sources: 260, wantSources: "260", wantPages: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestDB(t)
			root := filepath.Join(t.TempDir(), "projects")
			writeClaudeCorpus(t, root, tc.sources)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
				Machine:   "local",
			})
			t.Cleanup(engine.Close)
			logs := captureOperationLogs(t)

			require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(t.Context(), nil, true))

			records := operationLogRecords(t, logs.take())
			require.NotEmpty(t, records, "recovery must log its operation and cumulative progress")
			id := records[0]["operation"]
			_, err := strconv.ParseUint(id, 10, 64)
			require.NoError(t, err)
			assert.Equal(t, []map[string]string{
				{
					"operation": id, "kind": "reconciliation", "agent": "all",
					"roots": "0", "paths": "0", "lost_events": "true", "full": "true",
					"state": "started", "phase": "waiting", "elapsed": "duration",
				},
				{
					"operation": id, "kind": "reconciliation", "agent": "all",
					"roots": "0", "paths": "0", "lost_events": "true", "full": "true",
					"state": "finished", "phase": "finalizing", "elapsed": "duration",
					"lock_wait": "duration", "running": "duration", "canceled": "false",
					"error": "false", "pass": "1", "pages": tc.wantPages, "sources_done": tc.wantSources,
					"sources_total": tc.wantSources, "total_known": "true", "updated": tc.wantSources,
					"unchanged": "0", "errors": "0", "deferred": "0",
				},
			}, records)
		})
	}
}

func TestReconciliationDiagnosticsIncludeNativeTitleFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      string
		wantPass  string
		wantPages string
	}{
		{name: "scoped", kind: "reconciliation", wantPass: "1", wantPages: "1"},
		{name: "grouped", kind: "grouped_reconciliation", wantPass: "2", wantPages: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, store := testjsonl.QoderTitleRoot(t)
			transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
			testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "First")
			roots := []string{t.TempDir(), t.TempDir()}
			paths := []string{
				transcript,
				writeClaudeProjectSession(t, roots[0], "project", "first"),
				writeClaudeProjectSession(t, roots[1], "project", "second"),
			}
			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentQoder: {root}, parser.AgentClaude: roots},
				Machine:   "local",
			})
			t.Cleanup(engine.Close)
			require.NoError(t, engine.SyncPathsContext(t.Context(), paths))
			require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o600))
			logs := captureOperationLogs(t)

			if tc.name == "scoped" {
				stats, _, err := engine.ReconcileWatchRootsWithStats(t.Context(), roots, false, nil)
				require.NoError(t, err)
				require.Equal(t, 1, stats.Failed)
			} else {
				require.NoError(t, engine.ReconcileProviderRootsGrouped(t.Context(), []ProviderRootsGroup{
					{Agent: parser.AgentClaude, Roots: roots[:1]},
					{Agent: parser.AgentClaude, Roots: roots[1:]},
				}))
				require.Equal(t, 1, engine.LastSyncStats().Failed)
			}

			records := operationLogRecords(t, logs.take())
			require.NotEmpty(t, records)
			id := records[0]["operation"]
			assert.Equal(t, []map[string]string{
				{
					"operation": id, "kind": tc.kind, "agent": "all", "reasons": "none",
					"roots": "2", "paths": "0", "lost_events": "false", "full": "false",
					"state": "started", "phase": "waiting", "elapsed": "duration",
				},
				{
					"operation": id, "kind": tc.kind, "agent": "all", "reasons": "none",
					"roots": "2", "paths": "0", "lost_events": "false", "full": "false",
					"state": "finished", "phase": "finalizing", "elapsed": "duration",
					"lock_wait": "duration", "running": "duration", "canceled": "false",
					"error": "false", "pass": tc.wantPass, "pages": tc.wantPages,
					"sources_done": "2", "sources_total": "2", "total_known": "true",
					"updated": "0", "unchanged": "2", "errors": "1", "deferred": "0",
				},
			}, records)
		})
	}
}

func TestWatcherDiagnosticsPreservePostSyncError(t *testing.T) {
	database := openTestDB(t)
	root := filepath.Join(t.TempDir(), "projects")
	ids := writeClaudeCorpus(t, root, 1)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	logs := captureOperationLogs(t)
	sentinel := errors.New("post-sync work failed")
	_, err := engine.SyncWatchBatchThenRun(t.Context(), WatchBatch{
		Paths: []string{filepath.Join(root, "project", ids[0]+".jsonl")},
	}, nil, func() error { return sentinel })
	require.ErrorIs(t, err, sentinel)
	records := operationLogRecords(t, logs.take())
	require.NotEmpty(t, records)
	id := records[0]["operation"]
	assert.Equal(t, []map[string]string{
		{
			"operation": id, "kind": "watcher", "agent": "all",
			"roots": "0", "paths": "1", "lost_events": "false", "full": "false",
			"state": "started", "phase": "waiting", "elapsed": "duration",
		},
		{
			"operation": id, "kind": "watcher", "agent": "all",
			"roots": "0", "paths": "1", "lost_events": "false", "full": "false",
			"state": "finished", "phase": "post_sync", "elapsed": "duration",
			"lock_wait": "duration", "running": "duration", "canceled": "false",
			"error": "true", "pass": "0", "pages": "0", "sources_done": "0",
			"sources_total": "0", "total_known": "false", "updated": "1",
			"unchanged": "0", "errors": "0", "deferred": "0",
		},
	}, records)
}

func TestSyncOperationHeartbeatReportsOwnerAndStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := captureOperationLogs(t)
		engine := &Engine{}
		owner := engine.beginSyncOperation(t.Context(), syncOperationScope{kind: "watcher"})
		owner.acquiredLock()
		synctest.Wait()
		logs.take()

		time.Sleep(29 * time.Second)
		synctest.Wait()
		assert.Empty(t, logs.take(), "short operations need no heartbeat")

		waiter := engine.beginSyncOperation(t.Context(), syncOperationScope{kind: "reconciliation"})
		synctest.Wait()
		logs.take()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		records := operationLogRecords(t, logs.take())
		var waiting []map[string]string
		for _, record := range records {
			if record["kind"] == "reconciliation" {
				waiting = append(waiting, record)
			}
		}
		assert.Equal(t, []map[string]string{{
			"operation": strconv.FormatUint(waiter.id, 10), "kind": "reconciliation", "agent": "all",
			"roots": "0", "paths": "0", "lost_events": "false", "full": "false",
			"state": "heartbeat", "phase": "waiting", "elapsed": "duration",
			"lock_wait": "duration", "running": "duration", "canceled": "false",
			"error": "false", "pass": "0", "pages": "0", "sources_done": "0",
			"sources_total": "0", "total_known": "false", "updated": "0",
			"unchanged": "0", "errors": "0", "deferred": "0",
			"active_operation": strconv.FormatUint(owner.id, 10), "active_kind": "watcher",
		}}, waiting)

		owner.finish(t.Context(), nil)
		waiter.acquiredLock()
		waiter.beginReconciliation()
		waiter.discovered(260, true)
		waiter.beginPage()
		waiter.pageProgress(Progress{Phase: PhaseSyncing, SessionsDone: 256})
		waiter.beginPage()
		waiter.pageProgress(Progress{Phase: PhaseSyncing, SessionsDone: 4})
		logs.take()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		output := logs.take()
		assert.Contains(t, output, "lock_wait=30s running=30s")
		assert.Contains(t, output, "sources_done=260 sources_total=260")
		assert.NotContains(t, output, "active_operation=")
		assert.NotContains(t, output, "kind=watcher")

		waiter.finish(t.Context(), nil)
		logs.take()
		time.Sleep(time.Minute)
		synctest.Wait()
		assert.Empty(t, logs.take(), "finished operations must stop their observers")
	})
}

func TestSyncOperationCancellationKeepsExclusiveWorkUnchanged(t *testing.T) {
	sentinel := errors.New("exclusive work failed")
	for _, tc := range []struct {
		name       string
		canceled   bool
		workError  error
		wantError  error
		wantCalled bool
		wantPhase  string
	}{
		{name: "canceled", canceled: true, wantError: context.Canceled, wantPhase: "waiting"},
		{name: "work error", workError: sentinel, wantError: sentinel, wantCalled: true, wantPhase: "processing"},
		{name: "success", wantCalled: true, wantPhase: "processing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureOperationLogs(t)
			engine := &Engine{}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			called := false
			err := engine.RunExclusiveCtx(ctx, func() error {
				called = true
				return tc.workError
			})
			require.ErrorIs(t, err, tc.wantError)
			assert.Equal(t, tc.wantCalled, called)
			records := operationLogRecords(t, logs.take())
			require.NotEmpty(t, records)
			id := records[0]["operation"]
			finished := map[string]string{
				"operation": id, "kind": "exclusive", "agent": "all",
				"roots": "0", "paths": "0", "lost_events": "false", "full": "false",
				"state": "finished", "phase": tc.wantPhase, "elapsed": "duration",
				"lock_wait": "duration", "running": "duration", "canceled": strconv.FormatBool(tc.canceled),
				"error": strconv.FormatBool(tc.wantError != nil), "pass": "0", "pages": "0", "sources_done": "0",
				"sources_total": "0", "total_known": "false", "updated": "0",
				"unchanged": "0", "errors": "0", "deferred": "0",
			}
			if tc.canceled {
				finished["active_operation"], finished["active_kind"] = "0", "untracked"
			}
			assert.Equal(t, []map[string]string{
				{
					"operation": id, "kind": "exclusive", "agent": "all",
					"roots": "0", "paths": "0", "lost_events": "false", "full": "false",
					"state": "started", "phase": "waiting", "elapsed": "duration",
				},
				finished,
			}, records)
		})
	}
}

type diagnosticCountFailureSpool struct {
	reconciliationSpoolStore
}

func (spool *diagnosticCountFailureSpool) Count(context.Context) (int, error) {
	return 0, errors.New("diagnostic source count unavailable")
}

func TestRecoveryDiagnosticCountFailureDoesNotAbortSync(t *testing.T) {
	database := openTestDB(t)
	root := filepath.Join(t.TempDir(), "projects")
	ids := writeClaudeCorpus(t, root, 1)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	factory := engine.reconciliationSpoolFactory
	engine.reconciliationSpoolFactory = func(ctx context.Context, path string) (reconciliationSpoolStore, error) {
		spool, err := factory(ctx, path)
		if err != nil {
			return nil, err
		}
		return &diagnosticCountFailureSpool{reconciliationSpoolStore: spool}, nil
	}
	logs := captureOperationLogs(t)
	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(t.Context(), nil, true))
	session, err := database.GetSession(t.Context(), ids[0])
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Contains(t, logs.take(), "sources_done=1 sources_total=0 total_known=false updated=1")
}
