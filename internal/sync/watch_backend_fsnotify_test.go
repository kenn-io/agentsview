package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func testFSNotifyBackend(t *testing.T) *fsnotifyBackend {
	t.Helper()
	backend, err := newFSNotifyBackend(nil)
	require.NoError(t, err)
	t.Cleanup(backend.Stop)
	return backend
}

func bindRuntimePollingRecorder(t *testing.T, backend *fsnotifyBackend) (*[]PollingObligation, *[]string) {
	t.Helper()
	var added []PollingObligation
	var released []string
	backend.events = make(chan backendEvent, 16)
	backend.bindPollingOwnership(func(obligation PollingObligation) error {
		require.True(t, backend.pollingMu.TryLock(), "polling acquisition must release the polling lock")
		backend.pollingMu.Unlock()
		added = append(added, obligation)
		return nil
	}, func(key string) error {
		root := strings.TrimPrefix(key, "fsnotify-runtime:")
		for _, candidate := range backend.roots {
			if strings.HasPrefix(key, "fsnotify-folder:"+candidate+":") {
				root = candidate
				break
			}
		}
		if _, degraded := backend.degradedRoots[root]; degraded {
			released = append(released, key)
			return nil
		}
		var scopes []PollingScope
		for _, obligation := range added {
			if obligation.Key == key {
				scopes = obligation.Scopes
				break
			}
		}
		require.NotEmpty(t, scopes)
		for _, scope := range scopes {
			select {
			case event := <-backend.Events():
				assert.Equal(t, backendEvent{Path: scope.Root, Root: scope.Root, Op: backendOpReconcileRootChange, ItemType: backendItemDirectory}, event)
			default:
				return errors.New("polling released before folder reconcile")
			}
		}
		released = append(released, key)
		return nil
	})
	return &added, &released
}

func TestFSNotifyBackendVanishedRuntimeFolderSkipsPolling(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		remove bool
	}{
		{name: "walk"},
		{name: "add", err: os.ErrNotExist, remove: true},
		{name: "access denied", err: os.ErrPermission, remove: true},
		{name: "sharing violation", err: syscall.Errno(32), remove: true},
		{name: "delete pending", err: syscall.Errno(303), remove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "delete pending" && runtime.GOOS != "windows" {
				t.Skip("Windows delete-pending error")
			}
			backend := testFSNotifyBackend(t)
			root := filepath.Clean(t.TempDir())
			require.NoError(t, backend.AddRecursive(root, 8).Err)
			added, _ := bindRuntimePollingRecorder(t, backend)
			folder := filepath.Join(root, "vanished")
			if tc.err != nil {
				require.NoError(t, os.Mkdir(folder, 0o755))
				backend.watchOps = &failPathWatchOps{watchOps: backend.watchOps, failPath: folder, err: tc.err, beforeFail: func() {
					if tc.remove {
						assert.NoError(t, os.Remove(folder))
					}
				}}
			}
			backend.watchMu.Lock()
			assert.Empty(t, backend.addRuntimeSubtreeLocked(folder, []string{root}))
			backend.watchMu.Unlock()
			sibling := filepath.Join(root, "sibling")
			require.NoError(t, os.Mkdir(sibling, 0o755))
			backend.watchCreatedPath(sibling)
			assert.Empty(t, *added)
			assert.Contains(t, backend.watcher.WatchList(), sibling)
			assert.Contains(t, backend.watcher.WatchList(), root)
		})
	}
}

func TestFSNotifyBackendDeletePendingRetriesBeforePolling(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows delete-pending errors")
	}
	for _, cause := range []error{syscall.Errno(32), syscall.Errno(303)} {
		t.Run(cause.Error(), func(t *testing.T) {
			backend := testFSNotifyBackend(t)
			root := filepath.Clean(t.TempDir())
			require.NoError(t, backend.AddRecursive(root, 8).Err)
			added, _ := bindRuntimePollingRecorder(t, backend)
			folder := filepath.Join(root, "deleting")
			require.NoError(t, os.Mkdir(folder, 0o755))
			attempts := 0
			backend.watchOps = &failPathWatchOps{watchOps: backend.watchOps, failPath: folder, err: cause, beforeFail: func() {
				attempts++
				if attempts == 2 {
					require.NoError(t, os.Remove(folder))
				}
			}}
			backend.watchCreatedPath(folder)
			assert.Equal(t, 2, attempts)
			assert.Empty(t, *added)
			assert.Empty(t, backend.runtimeRoots)
		})
	}
}

func TestFSNotifyBackendDeferredReplacementKeepsPolling(t *testing.T) {
	for _, replacement := range []string{"file", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			backend := testFSNotifyBackend(t)
			root := filepath.Clean(t.TempDir())
			backend.setWatchRootPlan([]WatchRoot{{Path: root, Scopes: []WatchScope{{Agent: "claude", SyncDir: root}}}})
			require.NoError(t, backend.AddRecursive(root, 8).Err)
			added, released := bindRuntimePollingRecorder(t, backend)
			folder := filepath.Join(root, "project")
			require.NoError(t, os.Mkdir(folder, 0o755))
			backend.watchOps = &failPathWatchOps{watchOps: backend.watchOps, failPath: folder, err: os.ErrPermission}
			backend.watchCreatedPath(folder)
			require.Len(t, *added, 1)
			require.NoError(t, os.Remove(folder))
			if replacement == "file" {
				require.NoError(t, os.WriteFile(folder, nil, 0o600))
			} else {
				require.NoError(t, os.Symlink(t.TempDir(), folder))
			}
			assert.True(t, backend.retryRuntimeFolders(time.Now().Add(11*time.Second)))
			assert.Empty(t, *released)
			assert.NotContains(t, backend.watcher.WatchList(), folder)
			if replacement == "symlink" {
				database := openTestDB(t)
				emitter := &fakeEmitter{}
				engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local", Emitter: emitter})
				t.Cleanup(engine.Close)
				for _, name := range []string{"after-link", "later-write"} {
					writeClaudeProjectSession(t, root, "project", name)
					for _, scope := range (*added)[0].Scopes {
						require.NoError(t, engine.ReconcileProviderRoots(t.Context(), parser.AgentType(scope.Agent), []string{scope.Root}))
					}
					messages, err := database.GetMessages(t.Context(), name, 0, 10, true)
					require.NoError(t, err)
					require.Len(t, messages, 1)
					assert.Equal(t, "hi "+name, messages[0].Content)
				}
				assert.Equal(t, []string{"sessions", "sessions"}, emitter.got())
			}
			require.NoError(t, os.Remove(folder))
			assert.True(t, backend.retryRuntimeFolders(time.Now().Add(time.Minute)))
			assert.Equal(t, []string{"fsnotify-folder:" + root + ":" + folder}, *released)
		})
	}
}

func TestFSNotifyBackendRuntimeFailureWithoutPollingCallback(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := filepath.Clean(t.TempDir())
	require.NoError(t, backend.AddRecursive(root, 8).Err)
	folder := filepath.Join(root, "unreadable")
	require.NoError(t, os.Mkdir(folder, 0o755))
	backend.watchOps = &failPathWatchOps{watchOps: backend.watchOps, failPath: folder, err: os.ErrPermission}
	backend.watchCreatedPath(folder)
	assert.ErrorIs(t, <-backend.Errors(), os.ErrPermission)
	assert.True(t, backend.retryRuntimeFolders(time.Now().Add(time.Minute)))
	assert.Empty(t, backend.runtimeRoots)
	assert.Empty(t, backend.errors)
}

func TestFSNotifyBackendRuntimeFolderRecovery(t *testing.T) {
	for _, recovery := range []string{"watchable", "create", "gone", "release error"} {
		t.Run(recovery, func(t *testing.T) {
			backend := testFSNotifyBackend(t)
			root := filepath.Clean(t.TempDir())
			backend.setWatchRootPlan([]WatchRoot{{Path: root, Scopes: []WatchScope{{Agent: "claude", SyncDir: root}}}})
			require.NoError(t, backend.AddRecursive(root, 8).Err)
			added, released := bindRuntimePollingRecorder(t, backend)
			releaseErr := errors.New("poll coordinator stopped")
			recordRelease := backend.onPollingReleased
			if recovery == "release error" {
				backend.onPollingReleased = func(key string) error {
					folder := strings.TrimPrefix(key, "fsnotify-folder:"+root+":")
					assert.Equal(t, backendEvent{Path: folder, Root: folder, Op: backendOpReconcileRootChange, ItemType: backendItemDirectory}, <-backend.Events())
					return releaseErr
				}
			}
			ops := &scriptedWatchOps{fail: make(map[string]error)}
			backend.watchOps = ops
			a := filepath.Join(root, "a")
			b := filepath.Join(root, "b")
			for _, folder := range []string{a, b} {
				require.NoError(t, os.MkdirAll(folder, 0o755))
				ops.setFail(folder, os.ErrPermission)
				backend.watchCreatedPath(folder)
			}
			require.Equal(t, []PollingObligation{
				{Key: "fsnotify-folder:" + root + ":" + a, Scopes: []PollingScope{{Agent: "claude", Root: a}}, Probe: a},
				{Key: "fsnotify-folder:" + root + ":" + b, Scopes: []PollingScope{{Agent: "claude", Root: b}}, Probe: b},
			}, *added)
			attempts := ops.attemptCount()
			assert.True(t, backend.retryRuntimeFolders(time.Now().Add(9*time.Second)))
			assert.Equal(t, attempts, ops.attemptCount(), "no Add may run before the retry deadline")
			ops.setFail(a, nil)
			now := time.Now().Add(11 * time.Second)
			assert.True(t, backend.retryRuntimeFolders(now))
			assert.Equal(t, 1, ops.addCount(a))
			if recovery == "release error" {
				assert.Empty(t, *released)
				require.ErrorIs(t, <-backend.Errors(), releaseErr)
				assert.Len(t, backend.runtimeRoots[root].folders, 2)
				backend.onPollingReleased = recordRelease
			}
			if recovery != "release error" {
				assert.Equal(t, []string{"fsnotify-folder:" + root + ":" + a}, *released)
			}
			assert.Empty(t, backend.events)
			switch recovery {
			case "watchable", "create", "release error":
				ops.setFail(b, nil)
				child := filepath.Join(b, "nested")
				require.NoError(t, os.Mkdir(child, 0o755))
			default:
				require.NoError(t, os.Remove(b))
			}
			if recovery == "create" {
				require.NoError(t, os.RemoveAll(b))
				require.NoError(t, os.MkdirAll(filepath.Join(b, "nested"), 0o755))
				backend.translateEvent(fsnotify.Event{Name: b, Op: fsnotify.Create})
			}
			assert.True(t, backend.retryRuntimeFolders(now.Add(20*time.Second)))
			assert.Equal(t, []string{"fsnotify-folder:" + root + ":" + a, "fsnotify-folder:" + root + ":" + b}, *released)
			if recovery == "watchable" || recovery == "create" || recovery == "release error" {
				assert.Equal(t, 1, ops.addCount(b))
				assert.Equal(t, 1, ops.addCount(filepath.Join(b, "nested")))
			}
			assert.True(t, backend.retryRuntimeFolders(now.Add(3*time.Minute)))
			assert.Len(t, *released, 2)
			assert.Empty(t, backend.events)
		})
	}
}

func TestFSNotifyBackendRuntimeFailureKeepsSiblingWatches(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := filepath.Clean(t.TempDir())
	require.NoError(t, backend.AddRecursive(root, 8).Err)
	added, released := bindRuntimePollingRecorder(t, backend)
	folder := filepath.Join(root, "failed")
	require.NoError(t, os.Mkdir(folder, 0o755))
	backend.watchOps = &failPathWatchOps{watchOps: backend.watchOps, failPath: folder, err: os.ErrPermission}
	backend.watchCreatedPath(folder)
	sibling := filepath.Join(root, "sibling")
	require.NoError(t, os.Mkdir(sibling, 0o755))
	backend.watchCreatedPath(sibling)
	assert.Contains(t, backend.watcher.WatchList(), sibling)
	assert.Contains(t, backend.watcher.WatchList(), root)
	require.Len(t, *added, 1)
	assert.Equal(t, PollingObligation{Key: "fsnotify-folder:" + root + ":" + folder, Scopes: []PollingScope{{Root: root}}, Probe: folder}, (*added)[0])
	now := time.Now().Add(11 * time.Second)
	for _, delay := range []time.Duration{20 * time.Second, 40 * time.Second, 80 * time.Second, 2 * time.Minute, 2 * time.Minute} {
		assert.True(t, backend.retryRuntimeFolders(now))
		assert.True(t, backend.retryRuntimeFolders(now.Add(delay-time.Second)))
		backend.watchMu.Lock()
		state, ok := backend.runtimeRoots[root]
		var retryAt time.Time
		if ok {
			retryAt = state.retryAt
		}
		backend.watchMu.Unlock()
		require.True(t, ok)
		assert.Equal(t, now.Add(delay), retryAt)
		now = now.Add(delay)
	}
	assert.Empty(t, *released)
}

func TestFSNotifyBackendFolderPollingReconcilesOnlyFailedSubtree(t *testing.T) {
	for _, phase := range []string{"startup", "runtime"} {
		t.Run(phase, func(t *testing.T) {
			backend := testFSNotifyBackend(t)
			root := filepath.Clean(t.TempDir())
			folder := filepath.Join(root, "failed")
			sibling := filepath.Join(root, "sibling")
			writeClaudeProjectSession(t, root, "sibling", "sibling-session")
			backend.setWatchRootPlan([]WatchRoot{{Path: root, Scopes: []WatchScope{
				{Agent: "claude", SyncDir: root},
				{Agent: "cursor", SyncDir: sibling},
			}}})
			added, released := bindRuntimePollingRecorder(t, backend)
			ops := backend.watchOps
			backend.watchOps = &failPathWatchOps{watchOps: ops, failPath: folder, err: os.ErrPermission}
			if phase == "runtime" {
				require.NoError(t, backend.AddRecursive(root, 8).Err)
			}
			writeClaudeProjectSession(t, root, "failed", "failed-session")
			if phase == "startup" {
				result := backend.AddRecursive(root, 8)
				require.NoError(t, result.Err)
				assert.Zero(t, result.Unwatched, "folder polling already owns the failed watch")
			} else {
				backend.watchCreatedPath(folder)
			}
			require.Equal(t, []PollingObligation{{Key: "fsnotify-folder:" + root + ":" + folder, Scopes: []PollingScope{{Agent: "claude", Root: folder}, {Agent: "cursor", Root: sibling}}, Probe: folder}}, *added)
			assert.Contains(t, backend.watcher.WatchList(), root)
			assert.Contains(t, backend.watcher.WatchList(), sibling)
			assert.NotContains(t, backend.watcher.WatchList(), folder)

			backend.watchOps = ops
			assert.True(t, backend.retryRuntimeFolders(time.Now().Add(11*time.Second)))
			assert.Equal(t, []string{"fsnotify-folder:" + root + ":" + folder}, *released)
			assert.Contains(t, backend.watcher.WatchList(), folder)
			assert.Contains(t, backend.watcher.WatchList(), sibling)
			assert.Contains(t, backend.watcher.WatchList(), root)
		})
	}
}

func TestFSNotifyBackendNestedClaudeFailurePollsProject(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := filepath.Clean(t.TempDir())
	backend.setWatchRootPlan([]WatchRoot{{Path: root, Scopes: []WatchScope{{Agent: "claude", SyncDir: root}}}})
	require.NoError(t, backend.AddRecursive(root, 8).Err)
	folder := filepath.Join(root, "project", "owner", "subagents")
	require.NoError(t, os.MkdirAll(folder, 0o755))
	backend.watchOps = &failPathWatchOps{watchOps: backend.watchOps, failPath: folder, err: os.ErrPermission}
	added, _ := bindRuntimePollingRecorder(t, backend)
	backend.watchCreatedPath(filepath.Join(root, "project"))
	require.Len(t, *added, 1)
	assert.Equal(t, []PollingScope{{Agent: "claude", Root: filepath.Join(root, "project")}}, (*added)[0].Scopes)
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	writeClaudeProjectSession(t, root, "project", "owner")
	for _, scope := range (*added)[0].Scopes {
		require.NoError(t, engine.ReconcileProviderRoots(t.Context(), parser.AgentType(scope.Agent), []string{scope.Root}))
	}
	messages, err := database.GetMessages(t.Context(), "owner", 0, 10, true)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "hi owner", messages[0].Content)
}

func TestFSNotifyBackendCompanionPollingUpdatesOwnerThroughRecovery(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	project := filepath.Join(root, "project")
	folder := filepath.Join(project, "owner", "tool-results")
	require.NoError(t, os.MkdirAll(folder, 0o755))
	output := filepath.Join(folder, "output.txt")
	require.NoError(t, os.WriteFile(output, []byte("before"), 0o600))
	placeholder, err := json.Marshal("<persisted-output>\nOutput too large. Full output saved to: " + output + "\n</persisted-output>")
	require.NoError(t, err)
	outputJSON, err := json.Marshal(output)
	require.NoError(t, err)
	transcript := `{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"}}` + "\n" +
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"echo example"}}]}}` + "\n" +
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + string(placeholder) + `}]},"toolUseResult":{"persistedOutputPath":` + string(outputJSON) + `}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(project, "owner.jsonl"), []byte(transcript), 0o600))
	database := openTestDB(t)
	emitter := &fakeEmitter{}
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentIcodemate: {root}}, Machine: "local", Emitter: emitter})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	backend := testFSNotifyBackend(t)
	backend.setWatchRootPlan([]WatchRoot{{Path: root, Scopes: []WatchScope{{Agent: "icodemate", SyncDir: root}}}})
	ops := backend.watchOps
	backend.watchOps = &failPathWatchOps{watchOps: ops, failPath: folder, err: os.ErrPermission}
	added, released := bindRuntimePollingRecorder(t, backend)
	require.NoError(t, backend.AddRecursive(root, 8).Err)
	require.Len(t, *added, 1)
	assert.Contains(t, backend.watcher.WatchList(), project)
	assert.NotContains(t, backend.watcher.WatchList(), folder)
	assertOwner := func(want string) {
		t.Helper()
		messages, err := database.GetMessages(t.Context(), "icodemate:owner", 0, 10, true)
		require.NoError(t, err)
		require.Len(t, messages, 2)
		require.Len(t, messages[1].ToolCalls, 1)
		assert.Equal(t, want, messages[1].ToolCalls[0].ResultContent)
		assert.Contains(t, emitter.got(), "sessions")
	}
	assertOwner("before")
	emitter.mu.Lock()
	emitter.scopes = nil
	emitter.mu.Unlock()
	require.NoError(t, os.WriteFile(output, []byte("during failure"), 0o600))
	for _, scope := range (*added)[0].Scopes {
		require.NoError(t, engine.ReconcileProviderRoots(t.Context(), parser.AgentType(scope.Agent), []string{scope.Root}))
	}
	assertOwner("during failure")
	emitter.mu.Lock()
	emitter.scopes = nil
	emitter.mu.Unlock()
	require.NoError(t, os.WriteFile(output, []byte("on recovery"), 0o600))
	backend.watchOps = ops
	backend.onPollingReleased = func(key string) error {
		event := <-backend.Events()
		require.NoError(t, engine.ReconcileProviderRoots(t.Context(), parser.AgentIcodemate, []string{event.Root}))
		*released = append(*released, key)
		return nil
	}
	assert.True(t, backend.retryRuntimeFolders(time.Now().Add(11*time.Second)))
	assertOwner("on recovery")
	assert.Len(t, *released, 1)
	assert.Contains(t, backend.watcher.WatchList(), folder)
}

func TestFSNotifyBackendRuntimeStoppedWalkCoversSiblings(t *testing.T) {
	for _, cause := range []error{syscall.ENOSPC, os.ErrPermission, nil} {
		name := "budget"
		if cause != nil {
			name = cause.Error()
		}
		t.Run(name, func(t *testing.T) {
			backend := testFSNotifyBackend(t)
			root := filepath.Clean(t.TempDir())
			require.NoError(t, backend.AddRecursive(root, 2).Err)
			added, _ := bindRuntimePollingRecorder(t, backend)
			moved := filepath.Join(root, "incoming")
			a := filepath.Join(moved, "a")
			b := filepath.Join(moved, "b")
			require.NoError(t, os.MkdirAll(a, 0o755))
			require.NoError(t, os.MkdirAll(b, 0o755))
			if cause != nil {
				backend.runtimeBudget = 8
				backend.watchOps = &failPathWatchOps{watchOps: backend.watchOps, failPath: a, err: cause}
			}
			backend.watchCreatedPath(moved)
			assert.Contains(t, backend.watcher.WatchList(), moved)
			require.Len(t, *added, 1)
			probe := root
			if errors.Is(cause, os.ErrPermission) {
				probe = a
			}
			assert.Equal(t, probe, (*added)[0].Probe)
			if cause != nil {
				assert.Contains(t, backend.watcher.WatchList(), b)
			}
		})
	}
}

func TestFSNotifyBackendRuntimeFolderRecoveryPreservesRootDegradation(t *testing.T) {
	for _, cause := range []string{"overflow", "lost root"} {
		t.Run(cause, func(t *testing.T) {
			backend := testFSNotifyBackend(t)
			root := filepath.Clean(t.TempDir())
			require.NoError(t, backend.AddRecursive(root, 8).Err)
			added, released := bindRuntimePollingRecorder(t, backend)
			folder := filepath.Join(root, "failed")
			require.NoError(t, os.Mkdir(folder, 0o755))
			ops := backend.watchOps
			backend.watchOps = &failPathWatchOps{watchOps: ops, failPath: folder, err: os.ErrPermission}
			backend.watchCreatedPath(folder)
			if cause == "overflow" {
				require.True(t, backend.forwardNativeError(fsnotify.ErrEventOverflow))
				assert.Equal(t, backendOpFullSync, (<-backend.events).Op)
			} else {
				backend.translateEvent(fsnotify.Event{Name: root, Op: fsnotify.Remove})
			}
			backend.watchOps = ops
			assert.True(t, backend.retryRuntimeFolders(time.Now().Add(3*time.Minute)))
			assert.Len(t, *added, 2)
			assert.Equal(t, []string{"fsnotify-folder:" + root + ":" + folder}, *released)
			assert.Empty(t, backend.events)
		})
	}
}

func TestFSNotifyBackendRuntimeBudgetBoundsPollingAndLogging(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := filepath.Clean(t.TempDir())
	require.NoError(t, backend.AddRecursive(root, 1).Err)
	added, _ := bindRuntimePollingRecorder(t, backend)
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	for i := range 4096 {
		folder := filepath.Join(root, fmt.Sprintf("folder-%d", i))
		require.NoError(t, os.Mkdir(folder, 0o755))
		backend.watchCreatedPath(folder)
		backend.watchCreatedPath(folder)
	}
	assert.Len(t, *added, 1)
	assert.Contains(t, backend.degradedRoots, root)
	assert.Empty(t, backend.runtimeRoots, "budget exhaustion must not schedule whole-tree retries")
	assert.Equal(t, 1, strings.Count(output.String(), "fsnotify watch deferred under"))
	assert.True(t, backend.retryRuntimeFolders(time.Now().Add(11*time.Second)))
	assert.Len(t, *added, 1)
}

func TestFSNotifyBackendRuntimeRetryTimerReleasesPolling(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	folder := filepath.Join(root, "failed")
	require.NoError(t, os.Mkdir(folder, 0o755))
	synctest.Test(t, func(t *testing.T) {
		backend := testFSNotifyBackend(t)
		require.NoError(t, backend.watcher.Close())
		input := make(chan fsnotify.Event)
		backend.eventInput = input
		backend.errorInput = make(chan error)
		backend.watchOps = &failPathWatchOps{watchOps: alwaysAddWatchOps{}, failPath: folder, err: os.ErrPermission}
		defer backend.Stop()
		require.NoError(t, backend.AddRecursive(root, 8).Err)
		added := make(chan PollingObligation, 1)
		released := make(chan string, 1)
		backend.bindPollingOwnership(func(obligation PollingObligation) error {
			require.True(t, backend.pollingMu.TryLock(), "polling acquisition must release the polling lock")
			backend.pollingMu.Unlock()
			added <- obligation
			return nil
		}, func(key string) error {
			if !backend.pollingMu.TryLock() {
				return errors.New("polling release callback holds polling lock")
			}
			backend.pollingMu.Unlock()
			released <- key
			return nil
		})
		require.NoError(t, backend.Start())
		input <- fsnotify.Event{Name: folder, Op: fsnotify.Create}
		assert.Equal(t, backendOpCreate, requireReceiveWithin(t, backend.Events(), time.Second).Op)
		synctest.Wait()
		assert.Equal(t, "fsnotify-folder:"+root+":"+folder, (<-added).Key)
		backend.watchMu.Lock()
		backend.watchOps = alwaysAddWatchOps{}
		backend.watchMu.Unlock()
		time.Sleep(9 * time.Second)
		synctest.Wait()
		select {
		case event := <-backend.Events():
			require.FailNowf(t, "premature reconcile", "event: %+v", event)
		default:
		}
		assert.Empty(t, released)
		time.Sleep(time.Second)
		synctest.Wait()
		require.True(t, backend.pollingMu.TryLock(), "reconcile delivery must release the polling lock")
		backend.pollingMu.Unlock()
		assert.Equal(t, backendEvent{Path: root, Root: root, Op: backendOpReconcileRootChange, ItemType: backendItemDirectory}, requireReceiveWithin(t, backend.Events(), time.Second))
		synctest.Wait()
		assert.Equal(t, "fsnotify-folder:"+root+":"+folder, <-released)
	})
}

func TestNativeEventQueueSignalsDoNotProduceRetryItems(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queue := newNativeEventQueue()
		stop := make(chan struct{})
		defer close(stop)
		items := make(chan nativeItem, 1)
		go func() {
			for {
				item, ok := queue.next(stop)
				if !ok {
					return
				}
				items <- item
			}
		}()
		queue.push(nativeItem{event: fsnotify.Event{Name: "session.jsonl", Op: fsnotify.Write}})
		assert.Equal(t, "session.jsonl", (<-items).event.Name)
		synctest.Wait()
		assert.Empty(t, items, "an event signal must not schedule a retry pass")
		queue.push(nativeItem{retry: true})
		assert.True(t, (<-items).retry)
	})
}

func TestBufferedFSNotifyWatchOpsPreservesErrors(t *testing.T) {
	watcher, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	t.Cleanup(func() { _ = watcher.Close() })
	ops := bufferedFSNotifyWatchOps{Watcher: watcher}
	require.ErrorIs(t, ops.Add(filepath.Join(filepath.Clean(t.TempDir()), "absent")), os.ErrNotExist)
	require.NoError(t, watcher.Close())
	require.ErrorIs(t, ops.Add(filepath.Clean(t.TempDir())), fsnotify.ErrClosed)
}

func TestFSNotifyBackendOverflowRequestsLostEventRecovery(t *testing.T) {
	backend := testFSNotifyBackend(t)
	errorInput := make(chan error, 1)
	backend.errorInput = errorInput
	require.NoError(t, backend.Start())
	errorInput <- fsnotify.ErrEventOverflow

	pending := newPendingWatchBatch(8, 1_000)
	event := requireReceiveWithin(t, backend.Events(), time.Second)
	pending.AddBackendEvent(event)
	batch, ok := pending.Take()

	require.True(t, ok)
	assert.Equal(t, WatchBatch{FullSync: true, LostEvents: true}, batch)
}

func TestFSNotifyBackendOrdinaryErrorRemainsAnError(t *testing.T) {
	backend := testFSNotifyBackend(t)
	errorInput := make(chan error, 1)
	backend.errorInput = errorInput
	require.NoError(t, backend.Start())
	sentinel := errors.New("ordinary fsnotify failure")
	errorInput <- sentinel

	select {
	case event := <-backend.Events():
		assert.Fail(t, "ordinary error emitted a watch event", "event: %+v", event)
	case err := <-backend.Errors():
		require.ErrorIs(t, err, sentinel)
	case <-time.After(time.Second):
		require.FailNow(t, "fsnotify backend did not emit ordinary error")
	}
	assert.Never(t, func() bool {
		select {
		case <-backend.Events():
			return true
		default:
			return false
		}
	}, 50*time.Millisecond, time.Millisecond,
		"ordinary errors must not request lost-event recovery")
}

func TestFSNotifyBackendRemoveShallowRootClearsOwnership(t *testing.T) {
	backend := testFSNotifyBackend(t)
	base := filepath.Clean(t.TempDir())
	removedRoot := filepath.Join(base, "shallow")
	require.NoError(t, os.Mkdir(removedRoot, 0o755))
	require.NoError(t, backend.AddShallow(removedRoot))
	require.NoError(t, backend.Remove(removedRoot))

	result := backend.AddRecursive(base, math.MaxInt)
	require.NoError(t, result.Err)
	newDir := filepath.Join(removedRoot, "new")
	require.NoError(t, os.Mkdir(newDir, 0o755))
	itemType, excluded := backend.watchCreatedPath(newDir)
	assert.Equal(t, backendItemDirectory, itemType)
	assert.False(t, excluded)
	assert.Contains(t, backend.watcher.WatchList(), newDir,
		"removed shallow ownership must not suppress recursive auto-watch")

	err := backend.Remove(removedRoot)
	require.ErrorIs(t, err, fsnotify.ErrNonExistentWatch)
}

func TestFSNotifyBackendRemoveRecursiveRootRemovesDescendantWatches(t *testing.T) {
	for _, coverage := range []string{"native", "deferred", "overflow", "deferred release error", "overflow release error", "missing release", "removed during acquire"} {
		t.Run(coverage, func(t *testing.T) {
			backend := testFSNotifyBackend(t)
			root := filepath.Clean(t.TempDir())
			descendant := filepath.Join(root, "child", "nested")
			require.NoError(t, os.MkdirAll(descendant, 0o755))
			result := backend.AddRecursive(root, math.MaxInt)
			require.NoError(t, result.Err)
			require.Equal(t, 3, result.Watched)
			added, _ := bindRuntimePollingRecorder(t, backend)
			var releaseKeys []string
			releaseErr := errors.New("poll coordinator stopped")
			backend.onPollingReleased = func(key string) error {
				locked := backend.watchMu.TryLock()
				if locked {
					backend.watchMu.Unlock()
				}
				require.True(t, locked)
				require.True(t, backend.pollingMu.TryLock())
				backend.pollingMu.Unlock()
				releaseKeys = append(releaseKeys, key)
				if strings.HasSuffix(coverage, "release error") {
					return releaseErr
				}
				return nil
			}
			if coverage == "missing release" {
				backend.onPollingReleased = nil
			}
			if coverage == "removed during acquire" {
				acquire := backend.onPollingRequired
				backend.onPollingRequired = func(obligation PollingObligation) error {
					require.NoError(t, acquire(obligation))
					return backend.Remove(root)
				}
			}
			if coverage != "native" {
				failed := filepath.Join(root, "failed")
				require.NoError(t, os.Mkdir(failed, 0o755))
				backend.watchOps = &failPathWatchOps{watchOps: backend.watchOps, failPath: failed, err: os.ErrPermission}
				backend.watchCreatedPath(failed)
				require.Len(t, *added, 1)
				if strings.HasPrefix(coverage, "overflow") {
					require.True(t, backend.forwardNativeError(fsnotify.ErrEventOverflow))
					assert.Equal(t, backendOpFullSync, (<-backend.events).Op)
				}
			}
			removeErr := backend.Remove(root)
			if coverage == "removed during acquire" {
				require.ErrorIs(t, removeErr, fsnotify.ErrNonExistentWatch)
			} else if strings.HasSuffix(coverage, "release error") {
				require.ErrorIs(t, removeErr, releaseErr)
			} else if coverage == "missing release" {
				require.ErrorContains(t, removeErr, "without polling release callback")
			} else {
				require.NoError(t, removeErr)
			}
			assert.Empty(t, backend.watcher.WatchList())
			require.NoError(t, backend.AddShallow(filepath.Clean(t.TempDir())), "release failure must leave the backend usable")
			assert.True(t, backend.retryRuntimeFolders(time.Now().Add(3*time.Minute)))
			if coverage == "native" || coverage == "missing release" {
				assert.Empty(t, releaseKeys)
			} else {
				expected := []string{"fsnotify-folder:" + root + ":" + filepath.Join(root, "failed")}
				if strings.HasPrefix(coverage, "overflow") {
					expected = append(expected, "fsnotify-runtime:"+root)
				}
				assert.Equal(t, expected, releaseKeys)
			}
			assert.Empty(t, backend.events)
		})
	}
}

func TestFSNotifyBackendRemovePreservesOverlappingRecursiveRoot(t *testing.T) {
	backend := testFSNotifyBackend(t)
	parent := filepath.Clean(t.TempDir())
	sibling := filepath.Join(parent, "sibling")
	child := filepath.Join(parent, "child")
	nested := filepath.Join(child, "nested")
	for _, path := range []string{sibling, nested} {
		require.NoError(t, os.MkdirAll(path, 0o755))
	}
	require.NoError(t, backend.AddRecursive(parent, math.MaxInt).Err)
	require.NoError(t, backend.AddRecursive(child, math.MaxInt).Err)

	require.NoError(t, backend.Remove(parent))
	watched := backend.watcher.WatchList()
	slices.Sort(watched)
	want := []string{child, nested}
	slices.Sort(want)
	assert.Equal(t, want, watched)
}

func TestFSNotifyBackendRemoveDoesNotInheritExcludedParentOwnership(t *testing.T) {
	backend, err := newFSNotifyBackend([]string{"venv"})
	require.NoError(t, err)
	t.Cleanup(backend.Stop)
	parent := filepath.Clean(t.TempDir())
	nestedRoot := filepath.Join(parent, "venv", "project")
	nestedDir := filepath.Join(nestedRoot, "sessions")
	require.NoError(t, os.MkdirAll(nestedDir, 0o755))

	require.NoError(t, backend.AddRecursive(parent, math.MaxInt).Err)
	require.NoError(t, backend.AddRecursive(nestedRoot, math.MaxInt).Err)
	require.NoError(t, backend.Remove(nestedRoot))
	assert.Equal(t, []string{parent}, backend.watcher.WatchList(),
		"excluded parent root must not own explicit nested-root watches")

	require.NoError(t, backend.Start())
	nestedFile := filepath.Join(nestedDir, "session.jsonl")
	require.NoError(t, os.WriteFile(nestedFile, []byte("x"), 0o644))
	assertBackendPathNotEmitted(t, backend.Events(), nestedFile, 100*time.Millisecond)
}

func TestFSNotifyBackendRemoveDoesNotInheritBudgetSkippedParentOwnership(t *testing.T) {
	backend := testFSNotifyBackend(t)
	parent := filepath.Clean(t.TempDir())
	nestedRoot := filepath.Join(parent, "nested")
	nestedDir := filepath.Join(nestedRoot, "sessions")
	require.NoError(t, os.MkdirAll(nestedDir, 0o755))

	result := backend.AddRecursive(parent, 1)
	require.NoError(t, result.Err)
	require.True(t, result.BudgetExhausted)
	require.NoError(t, backend.AddRecursive(nestedRoot, math.MaxInt).Err)
	require.NoError(t, backend.Remove(nestedRoot))
	assert.Equal(t, []string{parent}, backend.watcher.WatchList(),
		"budget-skipped parent root must not own explicit nested-root watches")
}

func TestFSNotifyBackendExcludesExistingLockFileEvents(t *testing.T) {
	backend, err := newFSNotifyBackend([]string{"*.lock*"})
	require.NoError(t, err)
	t.Cleanup(backend.Stop)

	root := filepath.Clean(t.TempDir())
	require.NoError(t, backend.AddRecursive(root, math.MaxInt).Err)
	lockPath := filepath.Join(root, "session.jsonl.events.lock.temporary.pending")
	normalPath := filepath.Join(root, "session.jsonl")

	for _, op := range []fsnotify.Op{
		fsnotify.Create,
		fsnotify.Write,
		fsnotify.Remove,
		fsnotify.Rename,
	} {
		event, relevant := backend.translateEvent(fsnotify.Event{
			Name: lockPath,
			Op:   op,
		})
		assert.False(t, relevant, "lock event should be ignored for op %v", op)
		assert.Equal(t, backendEvent{}, event)
	}

	event, relevant := backend.translateEvent(fsnotify.Event{
		Name: normalPath,
		Op:   fsnotify.Write,
	})
	assert.True(t, relevant)
	assert.Equal(t, filepath.Clean(normalPath), event.Path)
	assert.Equal(t, backendOpWrite, event.Op)
}

type blockingRemoveWatchOps struct {
	watchOps      fsnotifyWatchOps
	removeStarted chan struct{}
	allowRemove   chan struct{}
	addCalled     chan struct{}
	removeOnce    sync.Once
	addOnce       sync.Once
}

type failPathWatchOps struct {
	watchOps   fsnotifyWatchOps
	failPath   string
	err        error
	beforeFail func()
}

func (w *failPathWatchOps) Add(path string) error {
	if filepath.Clean(path) == filepath.Clean(w.failPath) {
		if w.beforeFail != nil {
			w.beforeFail()
		}
		return w.err
	}
	return w.watchOps.Add(path)
}

func (w *failPathWatchOps) Remove(path string) error {
	return w.watchOps.Remove(path)
}

func TestFSNotifyBackendRuntimeBudgetDegradesExactScopesToPolling(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := filepath.Clean(t.TempDir())
	syncDir := filepath.Join(root, "logical-sessions")
	polling := make(chan PollingObligation, 1)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 8, 1_000,
		WatcherOptions{OnPollingRequired: func(obligation PollingObligation) error {
			polling <- obligation
			return nil
		}},
	)
	require.NoError(t, err)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{Agent: "claude", SyncDir: syncDir}},
	}}, 1)
	require.Len(t, results, 1)
	require.Equal(t, 1, results[0].Watched)
	require.NoError(t, backend.Start())

	created := filepath.Join(root, "created-after-startup")
	require.NoError(t, os.Mkdir(created, 0o755))
	obligation := requireReceiveWithin(t, polling, time.Second)
	assert.NotEmpty(t, obligation.Key)
	assert.Equal(t, []PollingScope{{Agent: "claude", Root: syncDir}}, obligation.Scopes)
	assert.NotContains(t, backend.watcher.WatchList(), created)
}

func TestFSNotifyBackendRuntimeDirectoryChurnReclaimsWatchBudget(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := filepath.Clean(t.TempDir())
	result := backend.AddRecursive(root, 2)
	require.NoError(t, result.Err)
	require.Equal(t, 1, result.Watched)
	require.NoError(t, backend.Start())

	created := filepath.Join(root, "recreated")
	for iteration := range 2 {
		require.NoError(t, os.Mkdir(created, 0o755))
		waitForBackendEvent(t, backend.Events(), created, backendOpCreate)
		assert.Contains(t, backend.watcher.WatchList(), created)

		require.NoError(t, os.Remove(created))
		waitForBackendEvent(t, backend.Events(), created, backendOpRemove|backendOpRename)
		assert.NotContains(t, backend.watcher.WatchList(), created)
		backend.watchMu.Lock()
		_, retained := backend.watchOwners[created]
		budget := backend.runtimeBudget
		backend.watchMu.Unlock()
		assert.False(t, retained, "removed directory ownership must be pruned")
		assert.Equal(t, 1, budget,
			"removed native watch must return its runtime budget slot")
		if iteration == 1 {
			break
		}
	}
}

func TestFSNotifyBackendRootLossTransfersExactScopeToPolling(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := filepath.Clean(t.TempDir())
	child := filepath.Join(root, "ordinary-child")
	require.NoError(t, os.Mkdir(child, 0o755))
	syncDir := filepath.Join(root, "logical-sessions")
	polling := make(chan PollingObligation, 1)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 8, 1_000,
		WatcherOptions{OnPollingRequired: func(obligation PollingObligation) error {
			polling <- obligation
			return nil
		}},
	)
	require.NoError(t, err)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{Agent: "claude", SyncDir: syncDir}},
	}}, 4)
	require.Len(t, results, 1)
	require.Equal(t, 2, results[0].Watched)

	childEvent, relevant := backend.translateEvent(fsnotify.Event{
		Name: child, Op: fsnotify.Remove,
	})
	require.True(t, relevant)
	assert.Equal(t, backendItemDirectory, childEvent.ItemType)
	select {
	case obligation := <-polling:
		assert.Fail(t, "ordinary descendant loss must not degrade its configured root",
			"unexpected obligation: %+v", obligation)
	default:
	}

	event, relevant := backend.translateEvent(fsnotify.Event{
		Name: root, Op: fsnotify.Remove,
	})
	require.True(t, relevant)
	assert.Equal(t, backendItemDirectory, event.ItemType)
	obligation := requireReceiveWithin(t, polling, time.Second)
	assert.Equal(t, "fsnotify-runtime:"+root, obligation.Key)
	assert.Equal(t, []PollingScope{{Agent: "claude", Root: syncDir}}, obligation.Scopes)
	assert.Empty(t, backend.watcher.WatchList())
}

func waitForBackendEvent(
	t *testing.T,
	events <-chan backendEvent,
	path string,
	wantOp backendOp,
) backendEvent {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if filepath.Clean(event.Path) == filepath.Clean(path) && event.Op&wantOp != 0 {
				return event
			}
		case <-deadline.C:
			require.FailNowf(t, "test failed", "backend event %v for %s was not observed", wantOp, path)
		}
	}
}

func TestFSNotifyBackendRuntimeAddFailureDegradesExactScopesToPolling(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := filepath.Clean(t.TempDir())
	syncDir := filepath.Join(root, "logical-sessions")
	polling := make(chan PollingObligation, 1)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 8, 1_000,
		WatcherOptions{OnPollingRequired: func(obligation PollingObligation) error {
			polling <- obligation
			return nil
		}},
	)
	require.NoError(t, err)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{Agent: "claude", SyncDir: syncDir}},
	}}, 4)
	require.Len(t, results, 1)
	require.Equal(t, 1, results[0].Watched)

	created := filepath.Join(root, "unwatchable")
	require.NoError(t, os.Mkdir(created, 0o755))
	backend.watchOps = &failPathWatchOps{
		watchOps: backend.watchOps, failPath: created, err: syscall.ENOSPC,
	}
	itemType, excluded := backend.watchCreatedPath(created)
	assert.Equal(t, backendItemDirectory, itemType)
	assert.False(t, excluded)
	obligation := requireReceiveWithin(t, polling, time.Second)
	assert.Equal(t, []PollingScope{{Agent: "claude", Root: syncDir}}, obligation.Scopes)
	assert.NotContains(t, backend.watcher.WatchList(), created)
}

func TestFSNotifyBackendRuntimeDegradationPreservesOverlappingRootScopes(t *testing.T) {
	backend := testFSNotifyBackend(t)
	parent := filepath.Clean(t.TempDir())
	nested := filepath.Join(parent, "nested")
	require.NoError(t, os.Mkdir(nested, 0o755))
	parentScope := filepath.Join(parent, "parent-sessions")
	nestedScope := filepath.Join(parent, "nested-sessions")
	polling := make(chan PollingObligation, 2)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 8, 1_000,
		WatcherOptions{OnPollingRequired: func(obligation PollingObligation) error {
			polling <- obligation
			return nil
		}},
	)
	require.NoError(t, err)
	results := watcher.RegisterRoots([]WatchRoot{
		{
			Path: parent, Recursive: true, Exists: true,
			Scopes: []WatchScope{{Agent: "claude", SyncDir: parentScope}},
		},
		{
			Path: nested, Recursive: true, Exists: true,
			Scopes: []WatchScope{{Agent: "cursor", SyncDir: nestedScope}},
		},
		// Two native watches cover both roots: nested is inside parent, so the
		// nested root reuses the watch parent already installed instead of
		// charging the shared budget for it a second time.
	}, 2)
	require.Len(t, results, 2)
	require.Equal(t, 2, results[0].Allocated)
	require.Zero(t, results[1].Allocated)
	require.False(t, results[1].BudgetExhausted)
	require.Zero(t, backend.runtimeBudget)

	created := filepath.Join(nested, "created-after-startup")
	require.NoError(t, os.Mkdir(created, 0o755))
	itemType, excluded := backend.watchCreatedPath(created)
	assert.Equal(t, backendItemDirectory, itemType)
	assert.False(t, excluded)
	first := requireReceiveWithin(t, polling, time.Second)
	second := requireReceiveWithin(t, polling, time.Second)
	obligations := map[string][]PollingScope{
		first.Key:  first.Scopes,
		second.Key: second.Scopes,
	}
	assert.Equal(t, []PollingScope{{Agent: "claude", Root: parentScope}}, obligations["fsnotify-runtime:"+parent])
	assert.Equal(t, []PollingScope{{Agent: "cursor", Root: nestedScope}}, obligations["fsnotify-runtime:"+nested])
}

func TestFSNotifyBackendRuntimeCreateRecursivelyWatchesMovedSubtree(t *testing.T) {
	backend := testFSNotifyBackend(t)
	backend.excludes = normalizeExcludePatterns([]string{"*.lock"})
	parent := filepath.Clean(t.TempDir())
	root := filepath.Join(parent, "watched")
	source := filepath.Join(parent, "incoming")
	deepSource := filepath.Join(source, "nested")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.MkdirAll(deepSource, 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(source, "subagents"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(source, "a.lock"), nil, 0o600))
	result := backend.AddRecursive(root, 8)
	require.NoError(t, result.Err)
	require.Equal(t, 1, result.Watched)

	moved := filepath.Join(root, "moved")
	require.NoError(t, os.Rename(source, moved))
	itemType, excluded := backend.watchCreatedPath(moved)
	assert.Equal(t, backendItemDirectory, itemType)
	assert.False(t, excluded)
	assert.Contains(t, backend.watcher.WatchList(), filepath.Join(moved, "subagents"))
	require.NoError(t, backend.Start())

	deepFile := filepath.Join(moved, "nested", "session.jsonl")
	require.NoError(t, os.WriteFile(deepFile, []byte("changed"), 0o644))
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-backend.Events():
			if filepath.Clean(event.Path) == filepath.Clean(deepFile) {
				assert.NotEqual(t, backendOpUnknown, event.Op)
				return
			}
		case <-deadline.C:
			require.FailNow(t, "deep write under moved subtree was not observed")
		}
	}
}

func (w *blockingRemoveWatchOps) Add(path string) error {
	w.addOnce.Do(func() { close(w.addCalled) })
	return w.watchOps.Add(path)
}

func (w *blockingRemoveWatchOps) Remove(path string) error {
	w.removeOnce.Do(func() { close(w.removeStarted) })
	<-w.allowRemove
	return w.watchOps.Remove(path)
}

func TestFSNotifyBackendConcurrentAddWaitsForRemoveOwnershipDecision(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := filepath.Clean(t.TempDir())
	require.NoError(t, backend.AddShallow(root))
	barrier := &blockingRemoveWatchOps{
		watchOps:      backend.watchOps,
		removeStarted: make(chan struct{}),
		allowRemove:   make(chan struct{}),
		addCalled:     make(chan struct{}),
	}
	backend.watchOps = barrier

	removeErr := make(chan error, 1)
	go func() { removeErr <- backend.Remove(root) }()
	select {
	case <-barrier.removeStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "Remove did not reach native-watch barrier")
	}

	addErr := make(chan error, 1)
	go func() { addErr <- backend.AddShallow(root) }()
	addReachedWhileRemoveBlocked := false
	select {
	case <-barrier.addCalled:
		addReachedWhileRemoveBlocked = true
	case <-time.After(50 * time.Millisecond): //nolint:kennlint // absence check; the held Remove barrier keeps the native Add waiting
	}
	close(barrier.allowRemove)
	require.NoError(t, <-removeErr)
	require.NoError(t, <-addErr)
	assert.False(t, addReachedWhileRemoveBlocked,
		"native Add must wait for Remove's ownership decision")
	assert.Contains(t, backend.watcher.WatchList(), root,
		"the newly retained logical root must keep its native watch")
}

func assertBackendPathNotEmitted(
	t *testing.T, events <-chan backendEvent, path string, duration time.Duration,
) {
	t.Helper()
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if !assert.NotEqual(t, path, event.Path) {
				return
			}
		case <-deadline.C:
			return
		}
	}
}

func TestFSNotifyBackendLifecycleStopBeforeStartReturns(t *testing.T) {
	backend := testFSNotifyBackend(t)
	stopped := make(chan struct{})
	go func() {
		backend.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(watcherTestTimeout):
		require.FailNow(t, "fsnotify backend Stop blocked before Start")
	}
	require.NoError(t, backend.Start())
	backend.Stop()
}

func TestFSNotifyBackendLifecycleRepeatedStartAndStop(t *testing.T) {
	backend := testFSNotifyBackend(t)
	require.NoError(t, backend.Start())
	require.NoError(t, backend.Start())
	backend.Stop()
	backend.Stop()
}

func TestFSNotifyBackendLifecycleStartStopRace(t *testing.T) {
	for range 100 {
		backend := testFSNotifyBackend(t)
		start := make(chan struct{})
		startErr := make(chan error, 1)
		var calls sync.WaitGroup
		calls.Add(2)
		go func() {
			defer calls.Done()
			<-start
			startErr <- backend.Start()
		}()
		go func() {
			defer calls.Done()
			<-start
			backend.Stop()
		}()
		close(start)
		done := make(chan struct{})
		go func() {
			calls.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			require.FailNow(t, "fsnotify backend concurrent Start and Stop deadlocked")
		}
		require.NoError(t, <-startErr)
		backend.Stop()
	}
}

// alwaysAddWatchOps accepts every watch so a test can isolate traversal
// failures from watch-registration failures.
type alwaysAddWatchOps struct{}

func (alwaysAddWatchOps) Add(string) error    { return nil }
func (alwaysAddWatchOps) Remove(string) error { return nil }

// An unreadable directory leaves its descendants without native watches even
// though its own watch registered. Startup traversal must report that as
// degraded coverage so the owning logical root gains a polling obligation
// instead of appearing fully watched.
func TestFSNotifyBackendAddRecursiveCountsUnreadableSubtreeAsUnwatched(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("directory read permissions are not enforced on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	backend := testFSNotifyBackend(t)
	backend.watchOps = alwaysAddWatchOps{}
	root := filepath.Clean(t.TempDir())
	unreadable := filepath.Join(root, "unreadable")
	require.NoError(t, os.MkdirAll(filepath.Join(unreadable, "hidden"), 0o755))
	require.NoError(t, os.Chmod(unreadable, 0o000))
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	result := backend.AddRecursive(root, 100)

	require.NoError(t, result.Err)
	assert.Positive(t, result.Unwatched,
		"an unenumerable subtree must count as degraded coverage")
}

// serialNativeWatcher models fsnotify's Windows backend contract: one reader
// goroutine delivers events on an unbuffered channel and is the only thing
// that services Add and Remove requests, and it does so only between
// deliveries. A request issued while the reader is blocked delivering the
// next event completes only after that event is consumed.
type serialNativeWatcher struct {
	events   chan fsnotify.Event
	requests chan chan struct{}
	deliver  chan []fsnotify.Event
	stop     chan struct{}
	stopOnce sync.Once
}

func newSerialNativeWatcher() *serialNativeWatcher {
	return &serialNativeWatcher{
		events:   make(chan fsnotify.Event),
		requests: make(chan chan struct{}, 1),
		deliver:  make(chan []fsnotify.Event),
		stop:     make(chan struct{}),
	}
}

func (w *serialNativeWatcher) run() {
	for {
		select {
		case <-w.stop:
			return
		case reply := <-w.requests:
			close(reply)
		case batch := <-w.deliver:
			for _, event := range batch {
				select {
				case w.events <- event:
				case <-w.stop:
					return
				}
			}
		}
	}
}

func (w *serialNativeWatcher) Stop() { w.stopOnce.Do(func() { close(w.stop) }) }

func (w *serialNativeWatcher) Add(string) error    { return w.request() }
func (w *serialNativeWatcher) Remove(string) error { return w.request() }

func (w *serialNativeWatcher) request() error {
	reply := make(chan struct{})
	select {
	case w.requests <- reply:
	case <-w.stop:
		return errors.New("native watcher stopped")
	}
	select {
	case <-reply:
		return nil
	case <-w.stop:
		return errors.New("native watcher stopped")
	}
}

// A directory removal that arrives in the same native batch as another event
// must not deadlock the backend: the event loop calls Remove on the native
// watcher, and on Windows that call is serviced by the same goroutine that is
// blocked delivering the next event to the loop.
func TestFSNotifyBackendRemoveDuringBatchDoesNotDeadlockEventLoop(t *testing.T) {
	backend := testFSNotifyBackend(t)
	native := newSerialNativeWatcher()
	go native.run()
	t.Cleanup(native.Stop)
	backend.watchOps = native
	backend.eventInput = native.events

	root := filepath.Clean(t.TempDir())
	sub := filepath.Join(root, "sub")
	require.NoError(t, os.Mkdir(sub, 0o755))
	result := backend.AddRecursive(root, 8)
	require.NoError(t, result.Err)
	require.Equal(t, 2, result.Watched)
	require.NoError(t, backend.Start())

	file := filepath.Join(root, "session.jsonl")
	native.deliver <- []fsnotify.Event{
		{Name: sub, Op: fsnotify.Remove},
		{Name: file, Op: fsnotify.Write},
	}

	first := requireReceiveWithin(t, backend.Events(), 2*time.Second)
	assert.Equal(t, sub, first.Path)
	assert.Equal(t, backendOpRemove, first.Op)
	second := requireReceiveWithin(t, backend.Events(), 2*time.Second)
	assert.Equal(t, file, second.Path)
	assert.Equal(t, backendOpWrite, second.Op)

	stopped := make(chan struct{})
	go func() {
		backend.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "fsnotify backend Stop hung after a Remove during a native batch")
	}
}

// On Windows, fsnotify delivers events and services Add on the same
// goroutine, and registration installs watches before Start runs. An event
// arriving between registration Adds must not block the next Add until Start:
// the pump has to consume native events as soon as watches exist.
func TestFSNotifyBackendEventBeforeStartDoesNotBlockRegistration(t *testing.T) {
	backend := testFSNotifyBackend(t)
	native := newSerialNativeWatcher()
	go native.run()
	t.Cleanup(native.Stop)
	backend.watchOps = native
	backend.eventInput = native.events

	root := filepath.Clean(t.TempDir())
	sub := filepath.Join(root, "sub")
	require.NoError(t, os.Mkdir(sub, 0o755))

	file := filepath.Join(root, "session.jsonl")
	// Once deliver hands the batch to the native reader, the reader is
	// committed to the delivery and services no Add requests until the event
	// is consumed, exactly like a Windows event arriving mid-registration.
	native.deliver <- []fsnotify.Event{{Name: file, Op: fsnotify.Write}}

	registered := make(chan RecursiveWatchResult, 1)
	go func() { registered <- backend.AddRecursive(root, 8) }()
	select {
	case result := <-registered:
		require.NoError(t, result.Err)
		require.Equal(t, 2, result.Watched)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "AddRecursive blocked on a native event delivered before Start")
	}

	require.NoError(t, backend.Start())
	event := requireReceiveWithin(t, backend.Events(), 2*time.Second)
	assert.Equal(t, file, event.Path)
	assert.Equal(t, backendOpWrite, event.Op)
}

// Overflow drops raw native events before their watch-maintenance side
// effects run, so missed creates can leave recursive subtrees without native
// watches. A full sync recovers the data but not the watches, so the
// backend must hand recursive roots to polling.
func TestFSNotifyBackendOverflowTransfersRecursiveRootsToPolling(t *testing.T) {
	backend := testFSNotifyBackend(t)
	errorInput := make(chan error, 1)
	backend.errorInput = errorInput
	root := filepath.Clean(t.TempDir())
	require.NoError(t, backend.AddRecursive(root, 8).Err)
	obligations := make(chan PollingObligation, 1)
	backend.bindPollingOwnership(func(obligation PollingObligation) error {
		obligations <- obligation
		return nil
	}, func(string) error { return nil })
	require.NoError(t, backend.Start())

	errorInput <- fsnotify.ErrEventOverflow

	batch := requireReceiveWithin(t, backend.Events(), 2*time.Second)
	assert.Equal(t, backendOpFullSync, batch.Op)
	obligation := requireReceiveWithin(t, obligations, 2*time.Second)
	assert.Equal(t, "fsnotify-runtime:"+root, obligation.Key)
}

type scriptedWatchOps struct {
	mu       sync.Mutex
	fail     map[string]error
	added    []string
	attempts []string
}

func (o *scriptedWatchOps) Add(path string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.attempts = append(o.attempts, path)
	if err := o.fail[path]; err != nil {
		return err
	}
	o.added = append(o.added, path)
	return nil
}

func (o *scriptedWatchOps) Remove(string) error { return nil }

func (o *scriptedWatchOps) attemptCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.attempts)
}

func (o *scriptedWatchOps) setFail(path string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fail[path] = err
}

func (o *scriptedWatchOps) addCount(path string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	count := 0
	for _, added := range o.added {
		if added == path {
			count++
		}
	}
	return count
}

// A dropped shallow-root removal loses the native watch even though the
// one-time full sync recovers the data, so overflow must revalidate shallow
// coverage: re-add every shallow watch and hand roots that cannot be
// re-added to polling, exactly as an observed removal would.
func TestFSNotifyBackendOverflowReinstallsShallowWatches(t *testing.T) {
	backend := testFSNotifyBackend(t)
	errorInput := make(chan error, 1)
	backend.errorInput = errorInput
	ops := &scriptedWatchOps{fail: map[string]error{}}
	backend.watchOps = ops
	kept := filepath.Join(filepath.Clean(t.TempDir()), "kept")
	lost := filepath.Join(filepath.Clean(t.TempDir()), "lost")
	require.NoError(t, backend.AddShallow(kept))
	require.NoError(t, backend.AddShallow(lost))
	ops.setFail(lost, errors.New("directory removed"))
	obligations := make(chan PollingObligation, 2)
	backend.bindPollingOwnership(func(obligation PollingObligation) error {
		obligations <- obligation
		return nil
	}, func(string) error { return nil })
	require.NoError(t, backend.Start())

	errorInput <- fsnotify.ErrEventOverflow

	event := requireReceiveWithin(t, backend.Events(), 2*time.Second)
	assert.Equal(t, backendOpFullSync, event.Op)
	obligation := requireReceiveWithin(t, obligations, 2*time.Second)
	assert.Equal(t, "fsnotify-runtime:"+lost, obligation.Key)
	assert.Equal(t, 2, ops.addCount(kept),
		"a reachable shallow root must have its native watch re-added")
	select {
	case extra := <-obligations:
		require.FailNowf(t, "test failed", "re-addable shallow root was degraded to polling: %+v", extra)
	default:
	}
}

func TestNativeEventQueueOverflowSurfacesLostEventsOnce(t *testing.T) {
	queue := newNativeEventQueue()
	stop := make(chan struct{})
	for range nativeEventQueueLimit + 1 {
		queue.push(nativeItem{event: fsnotify.Event{Name: "before", Op: fsnotify.Write}})
	}
	queue.push(nativeItem{event: fsnotify.Event{Name: "after", Op: fsnotify.Write}})

	first, ok := queue.next(stop)
	require.True(t, ok)
	require.ErrorIs(t, first.err, fsnotify.ErrEventOverflow)
	remaining := []string{}
	for {
		item, ok := queue.next(stop)
		if !ok {
			break
		}
		require.NoError(t, item.err, "overflow must be reported once")
		remaining = append(remaining, item.event.Name)
		if item.event.Name == "after" {
			break
		}
	}
	assert.Equal(t, []string{"after"}, remaining, "events after the overflow survive")

	queue.close()
	_, ok = queue.next(stop)
	assert.False(t, ok, "a closed and drained queue reports no more items")
}

func TestFSNotifyBackendQueueOverflowRequestsFullSync(t *testing.T) {
	backend := testFSNotifyBackend(t)
	native := newSerialNativeWatcher()
	go native.run()
	t.Cleanup(native.Stop)
	backend.watchOps = native
	backend.eventInput = native.events
	require.NoError(t, backend.Start())

	root := filepath.Clean(t.TempDir())
	burst := make([]fsnotify.Event, 0, nativeEventQueueLimit+8)
	for range cap(burst) {
		burst = append(burst, fsnotify.Event{Name: filepath.Join(root, "f"), Op: fsnotify.Write})
	}
	native.deliver <- burst
	// The loop is parked on its first forward because nothing reads events
	// yet, so the pump alone must absorb the rest of the burst. A second
	// batch is accepted only once the first has been fully delivered.
	native.deliver <- nil

	sawFullSync := false
	deadline := time.After(5 * time.Second)
	for !sawFullSync {
		select {
		case event := <-backend.Events():
			sawFullSync = event.Op == backendOpFullSync
		case <-deadline:
			require.FailNow(t, "queue overflow did not request a full sync")
		}
	}
}
