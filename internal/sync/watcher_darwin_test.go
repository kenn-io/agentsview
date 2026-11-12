//go:build darwin && cgo

package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.kenn.io/agentsview/internal/fsevents"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/stacktest"
)

func TestFSEventsFlagMapping(t *testing.T) {
	const (
		mustScanSubDirs = uint32(0x00000001)
		userDropped     = uint32(0x00000002)
		kernelDropped   = uint32(0x00000004)
		eventIDsWrapped = uint32(0x00000008)
		rootChanged     = uint32(0x00000020)
		itemCreated     = uint32(0x00000100)
		itemRemoved     = uint32(0x00000200)
		inodeMetaMod    = uint32(0x00000400)
		itemRenamed     = uint32(0x00000800)
		itemModified    = uint32(0x00001000)
		itemIsFile      = uint32(0x00010000)
		itemIsDir       = uint32(0x00020000)
	)

	root := "/sessions"
	path := "/sessions/2026/07/session.jsonl"
	tests := []struct {
		name     string
		flags    uint32
		want     backendEvent
		relevant bool
		fallback bool
	}{
		{name: "ItemCreated", flags: itemCreated, want: backendEvent{Path: path, Root: root, Op: backendOpCreate}, relevant: true},
		{name: "ItemModified", flags: itemModified, want: backendEvent{Path: path, Root: root, Op: backendOpWrite}, relevant: true},
		{name: "ItemRemoved", flags: itemRemoved, want: backendEvent{Path: path, Root: root, Op: backendOpRemove}, relevant: true},
		{name: "ItemRenamed", flags: itemRenamed, want: backendEvent{Path: path, Root: root, Op: backendOpRename}, relevant: true},
		{name: "ItemIsFile", flags: itemRenamed | itemIsFile, want: backendEvent{Path: path, Root: root, Op: backendOpRename, ItemType: backendItemFile}, relevant: true},
		{name: "ItemIsDir", flags: itemRenamed | itemIsDir, want: backendEvent{Path: path, Root: root, Op: backendOpRename, ItemType: backendItemDirectory}, relevant: true},
		{name: "RootChanged", flags: rootChanged, want: backendEvent{Path: path, Root: root, Op: backendOpReconcileRootChange}, relevant: true},
		{name: "MustScanSubDirs", flags: mustScanSubDirs, fallback: true},
		{name: "UserDropped", flags: userDropped, fallback: true},
		{name: "KernelDropped", flags: kernelDropped, fallback: true},
		{name: "EventIdsWrapped", flags: eventIDsWrapped, fallback: true},
		{name: "dropped with operation", flags: userDropped | itemCreated | itemIsFile, fallback: true},
		{name: "metadata only", flags: inodeMetaMod, relevant: false},
		{name: "metadata with operation", flags: inodeMetaMod | itemCreated | itemIsFile, want: backendEvent{Path: path, Root: root, Op: backendOpCreate, ItemType: backendItemFile}, relevant: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := []fsevents.Event{{
				Path:  path,
				Flags: fsevents.EventFlags(tc.flags),
			}}

			assert.Equal(t, tc.fallback, nativeDeliveryRequestsFallback(events))
			if tc.fallback {
				// consumeFSEvents diverts drop and rescan flags to fallback
				// before translation, so these flags never reach
				// translateFSEvent.
				return
			}

			got, relevant := translateFSEvent(root, events[0])
			assert.Equal(t, tc.relevant, relevant)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDarwinWatcherNativeSinkCollapsesBlockedConsumerOverflow(t *testing.T) {
	backend := newDarwinDirectTestBackend()
	batches := make(chan WatchBatch, 3)
	consumerEntered := make(chan struct{})
	releaseConsumer := make(chan struct{})
	unblockConsumer := sync.OnceFunc(func() { close(releaseConsumer) })
	watcher, err := newWatcherWithBackend(
		0,
		0,
		func(_ context.Context, batch WatchBatch) error {
			batches <- batch
			if !batch.FullSync {
				close(consumerEntered)
				<-releaseConsumer
			}
			return nil
		},
		backend,
		defaultWatchBatchMaxEntries,
		defaultWatchBatchMaxPathBytes,
	)
	require.NoError(t, err)
	watcher.Start()
	t.Cleanup(watcher.Stop)
	t.Cleanup(unblockConsumer)

	require.True(t, backend.emit([]backendEvent{{
		Path: "/sessions/first.jsonl",
		Root: "/sessions",
		Op:   backendOpWrite,
	}}))
	requireReceiveWithin(t, consumerEntered, time.Second)

	const eventCount = defaultWatchBatchMaxEntries + 1024
	padding := strings.Repeat("x", 256)
	events := make([]backendEvent, 0, eventCount)
	for i := range eventCount {
		events = append(events, backendEvent{
			Path: fmt.Sprintf("/sessions/%05d-%s.jsonl", i, padding),
			Root: "/sessions",
			Op:   backendOpWrite,
		})
	}

	nativeReturned := make(chan bool, 1)
	go func() {
		nativeReturned <- backend.emit(events)
	}()
	assert.True(t, requireReceiveWithin(t, nativeReturned, time.Second),
		"native sink must acquire the idle pending accumulator")

	func() {
		watcher.eventSink.mu.Lock()
		defer watcher.eventSink.mu.Unlock()
		// The scheduler can absorb the handoff while the callback is blocked.
		watcher.eventSink.absorbHandoff()
		pending := watcher.eventSink.pending
		assert.True(t, pending.fullSync)
		assert.LessOrEqual(t, len(pending.strings), defaultWatchBatchMaxEntries)
		assert.LessOrEqual(t, pending.pathBytes, defaultWatchBatchMaxPathBytes)
	}()

	first := requireReceiveWithin(t, batches, time.Second)
	assert.Equal(t, WatchBatch{
		Paths:          []string{"/sessions/first.jsonl"},
		Renames:        []WatchRename{},
		ReconcileRoots: []string{},
	}, first)
	unblockConsumer()
	second := requireReceiveWithin(t, batches, time.Second)
	assert.Equal(t, WatchBatch{FullSync: true, LostEvents: true}, second)
	assert.Never(t, func() bool { return len(batches) != 0 }, 100*time.Millisecond, 10*time.Millisecond)
}

func TestDarwinWatcherNativeSinkContentionUsesBoundedHandoff(t *testing.T) {
	backend := newDarwinDirectTestBackend()
	batches := make(chan WatchBatch, 1)
	watcher, err := newWatcherWithBackend(
		0,
		0,
		func(_ context.Context, batch WatchBatch) error {
			batches <- batch
			return nil
		},
		backend,
		defaultWatchBatchMaxEntries,
		defaultWatchBatchMaxPathBytes,
	)
	require.NoError(t, err)
	watcher.Start()
	t.Cleanup(watcher.Stop)

	watcher.eventSink.mu.Lock()
	nativeReturned := make(chan bool, 1)
	go func() {
		nativeReturned <- backend.emit([]backendEvent{{
			Path: "/sessions/contended.jsonl",
			Root: "/sessions",
			Op:   backendOpWrite,
		}})
	}()
	assert.True(t, requireReceiveWithin(t, nativeReturned, time.Second),
		"a contended callback must not wait for accumulator ownership")
	watcher.eventSink.mu.Unlock()

	assert.Equal(t, WatchBatch{
		Paths:          []string{"/sessions/contended.jsonl"},
		Renames:        []WatchRename{},
		ReconcileRoots: []string{},
	},
		requireReceiveWithin(t, batches, time.Second))
	assert.Zero(t, watcher.eventSink.overflows.Load(),
		"ordinary consumer mutex contention is not a capacity overflow")
}

func TestDarwinWatcherMeaningfulNativeDeliveryAdvancesShallowTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const batchDelay = 300 * time.Millisecond
		backend := newDarwinDirectTestBackend()
		batches := make(chan WatchBatch, 1)
		watcher, err := newWatcherWithBackend(
			batchDelay,
			0,
			func(_ context.Context, batch WatchBatch) error {
				batches <- batch
				return nil
			},
			backend,
			defaultWatchBatchMaxEntries,
			defaultWatchBatchMaxPathBytes,
		)
		require.NoError(t, err)
		watcher.Start()
		t.Cleanup(watcher.Stop)

		backend.sendShallow(t, backendEvent{
			Path: "/sessions/shallow.jsonl",
			Root: "/sessions",
			Op:   backendOpWrite,
		})
		synctest.Wait()
		nativeAt := time.Now()
		require.True(t, backend.emit([]backendEvent{{
			Path: "/sessions/native.jsonl",
			Root: "/sessions",
			Op:   backendOpWrite,
		}}))

		batch := requireReceiveWithin(t, batches, 150*time.Millisecond)
		assert.ElementsMatch(t, []string{
			"/sessions/shallow.jsonl",
			"/sessions/native.jsonl",
		}, batch.Paths)
		assert.Less(t, time.Since(nativeAt), 150*time.Millisecond)
	})
}

func TestWatchEventSinkFilteredNativeDeliveryDoesNotWakePendingShallow(t *testing.T) {
	sink := newWatchEventSink(
		defaultWatchBatchMaxEntries,
		defaultWatchBatchMaxPathBytes,
	)
	sink.Add(backendEvent{
		Path: "/sessions/shallow.jsonl",
		Root: "/sessions",
		Op:   backendOpWrite,
	})

	require.True(t, sink.TryAccumulate(func(func(backendEvent) bool) bool {
		return false
	}))
	select {
	case <-sink.wake:
		require.Fail(t, "filtered native delivery woke unrelated shallow work")
	default:
	}
}

func TestDarwinWatcherFilteredNativeDeliveryPreservesShallowTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const batchDelay = 180 * time.Millisecond
		backend := newDarwinDirectTestBackend()
		dispatched := make(chan time.Time, 1)
		watcher, err := newWatcherWithBackend(
			batchDelay,
			0,
			func(_ context.Context, _ WatchBatch) error {
				dispatched <- time.Now()
				return nil
			},
			backend,
			defaultWatchBatchMaxEntries,
			defaultWatchBatchMaxPathBytes,
		)
		require.NoError(t, err)
		watcher.Start()
		t.Cleanup(watcher.Stop)

		queuedAt := time.Now()
		backend.sendShallow(t, backendEvent{
			Path: "/sessions/shallow.jsonl",
			Root: "/sessions",
			Op:   backendOpWrite,
		})
		synctest.Wait()
		require.True(t, backend.emitFiltered())

		select {
		case <-dispatched:
			require.Fail(t, "filtered native delivery advanced the shallow timer")
		case <-time.After(80 * time.Millisecond):
		}
		dispatchedAt := requireReceiveWithin(t, dispatched, 250*time.Millisecond)
		assert.GreaterOrEqual(t, dispatchedAt.Sub(queuedAt), 140*time.Millisecond)
	})
}

func TestDarwinWatcherFileLifecycleAndNativeLatency(t *testing.T) {
	root := t.TempDir()
	watcher, batches := newDarwinTestWatcher(t, root, nil, darwinFSEventsLatency)
	watcher.Start()

	path := filepath.Join(root, "session.jsonl")
	createdAt := time.Now()
	require.NoError(t, os.WriteFile(path, []byte("created"), 0o600))
	created := waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.Paths, path)
	})
	deliveryElapsed := time.Since(createdAt)
	t.Logf("500ms FSEvents create delivery elapsed: %s", deliveryElapsed.Round(time.Millisecond))
	assert.Less(t, deliveryElapsed, 900*time.Millisecond,
		"native latency must not be followed by the configured 500ms Go batch delay")
	assert.False(t, created.FullSync)

	require.NoError(t, os.WriteFile(path, []byte("modified"), 0o600))
	modified := waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.Paths, path)
	})
	assert.False(t, modified.FullSync)

	renamedPath := filepath.Join(root, "renamed.jsonl")
	require.NoError(t, os.Rename(path, renamedPath))
	renamed := waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.ContainsFunc(batch.Renames, func(rename WatchRename) bool {
			return rename.Path == path || rename.Path == renamedPath
		})
	})
	assert.False(t, renamed.FullSync)
	assert.Empty(t, renamed.ReconcileRoots)
	for _, rename := range renamed.Renames {
		assert.Equal(t, ItemIsFile, rename.ItemType)
	}

	require.NoError(t, os.Remove(renamedPath))
	removed := waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.Paths, renamedPath) ||
			slices.ContainsFunc(batch.Renames, func(rename WatchRename) bool {
				return rename.Path == renamedPath
			})
	})
	assert.False(t, removed.FullSync)
	assert.Empty(t, removed.ReconcileRoots)
}

func TestDarwinWatcherColdArchiveCardinalityUsesOneRecursiveStream(t *testing.T) {
	type observation struct {
		streams        int
		roots          int
		logicalRoots   int
		shallowRoots   int
		shallowStreams int
		appendPaths    int
		appendRenames  int
	}
	var observations []observation
	for _, tc := range []struct {
		name      string
		coldFiles int
	}{
		{name: "small", coldFiles: 3},
		{name: "large", coldFiles: 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			activeDir := filepath.Join(root, "active")
			require.NoError(t, os.Mkdir(activeDir, 0o700))
			changedPath := filepath.Join(activeDir, "changed-session.jsonl")
			require.NoError(t, os.WriteFile(
				changedPath, []byte("{\"type\":\"user\"}\n"), 0o600,
			))
			for i := range tc.coldFiles {
				dir := filepath.Join(root, fmt.Sprintf("cold-%03d", i))
				require.NoError(t, os.Mkdir(dir, 0o700))
				require.NoError(t, os.WriteFile(
					filepath.Join(dir, fmt.Sprintf("cold-%03d.jsonl", i)),
					[]byte("{\"type\":\"user\"}\n"),
					0o600,
				))
			}

			backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
			require.NoError(t, err)
			watcher, batches := newDarwinTestWatcherWithBackend(t, backend)
			results := watcher.RegisterRoots([]WatchRoot{{
				Path: root, Recursive: true, Exists: true,
				Scopes: []WatchScope{{Agent: "claude", SyncDir: root}},
			}}, 1)
			require.Len(t, results, 1)
			require.NoError(t, results[0].Err)
			require.Equal(t, 1, results[0].Watched)
			watcher.Start()
			// A post-start barrier separates any filesystem history FSEvents was
			// still coalescing from the append measured below.
			barrierPath := filepath.Join(root, ".stream-ready")
			require.NoError(t, os.WriteFile(barrierPath, []byte("ready"), 0o600))
			waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
				return slices.Contains(batch.Paths, barrierPath)
			})

			backend.mu.Lock()
			got := observation{
				streams:        len(backend.streams),
				roots:          len(backend.roots.Load().roots),
				logicalRoots:   len(backend.logical),
				shallowRoots:   len(backend.shallow),
				shallowStreams: backend.shallowWatcher.streamCount(),
			}
			backend.mu.Unlock()
			assert.Equal(t, 1, got.streams)
			assert.Equal(t, 1, got.roots)
			assert.Equal(t, 1, got.logicalRoots)
			assert.Zero(t, got.shallowRoots)
			assert.Zero(t, got.shallowStreams,
				"recursive FSEvents coverage must not open shallow streams")

			appendFile, err := os.OpenFile(changedPath, os.O_APPEND|os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = appendFile.WriteString("{\"type\":\"assistant\"}\n")
			require.NoError(t, err)
			var appendBatch WatchBatch
			observedBeforeClose := false
			observationTimer := time.NewTimer(750 * time.Millisecond) //nolint:kennlint // deadlinetest: absence check; the window only records whether the open append surfaces before close, and either outcome passes
		observeOpenAppend:
			for {
				select {
				case batch := <-batches:
					if slices.Contains(batch.Paths, changedPath) {
						appendBatch = batch
						observedBeforeClose = true
						break observeOpenAppend
					}
				case <-observationTimer.C:
					break observeOpenAppend
				}
			}
			if !observationTimer.Stop() {
				select {
				case <-observationTimer.C:
				default:
				}
			}
			require.NoError(t, appendFile.Close())
			if !observedBeforeClose {
				appendBatch = waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
					return slices.Contains(batch.Paths, changedPath)
				})
			}
			t.Logf("recursive append observed before descriptor close: %t",
				observedBeforeClose)
			assert.False(t, appendBatch.FullSync)
			assert.Empty(t, appendBatch.ReconcileRoots)
			assert.Empty(t, appendBatch.Renames)
			assert.Equal(t, []string{changedPath}, appendBatch.Paths)
			got.appendPaths = len(appendBatch.Paths)
			got.appendRenames = len(appendBatch.Renames)

			// Establish an after-close delivery barrier so any duplicate append
			// notification cannot be mistaken for the atomic replacement below.
			closedBarrier := filepath.Join(root, ".append-closed")
			require.NoError(t, os.WriteFile(closedBarrier, []byte("closed"), 0o600))
			waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
				return slices.Contains(batch.Paths, closedBarrier)
			})

			tempPath := filepath.Join(activeDir, ".changed-session.tmp")
			require.NoError(t, os.WriteFile(
				tempPath,
				[]byte("{\"type\":\"user\",\"replacement\":true}\n"),
				0o600,
			))
			require.NoError(t, os.Rename(tempPath, changedPath))
			replacementBatch := waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
				return slices.Contains(batch.Paths, changedPath) ||
					slices.ContainsFunc(batch.Renames, func(rename WatchRename) bool {
						return rename.Path == changedPath
					})
			})
			assert.False(t, replacementBatch.FullSync)
			assert.Empty(t, replacementBatch.ReconcileRoots,
				"atomic file replacement must remain a changed-path sync")
			// FSEvents can deliver the temporary file's create and rename in
			// separate events within this batch. A path may therefore appear
			// in both Paths and Renames; their combined count depends on native
			// coalescing, not archive size. Bound the affected paths instead.
			allowedPaths := []string{tempPath, changedPath}
			assert.Subset(t, allowedPaths, replacementBatch.Paths,
				"atomic replacement must not include cold archive paths")
			assert.LessOrEqual(t, len(replacementBatch.Paths), 2)
			assert.LessOrEqual(t, len(replacementBatch.Renames), 2)
			for _, rename := range replacementBatch.Renames {
				assert.Contains(t, allowedPaths, rename.Path,
					"atomic replacement must not include cold archive paths")
				assert.Equal(t, ItemIsFile, rename.ItemType)
			}

			assert.Never(t, func() bool {
				select {
				case batch := <-batches:
					return batch.FullSync || len(batch.ReconcileRoots) > 0
				default:
					return false
				}
			}, 250*time.Millisecond, 10*time.Millisecond,
				"atomic file replacement must not queue reconciliation")

			backend.mu.Lock()
			assert.Len(t, backend.streams, got.streams)
			assert.Len(t, backend.roots.Load().roots, got.roots)
			assert.Len(t, backend.logical, got.logicalRoots)
			assert.Len(t, backend.shallow, got.shallowRoots)
			assert.Equal(t, got.shallowStreams, backend.shallowWatcher.streamCount())
			backend.mu.Unlock()
			observations = append(observations, got)
		})
	}
	require.Len(t, observations, 2)
	assert.Equal(t, observations[0], observations[1],
		"recursive native state and append work must not scale with cold files or directories")
}

func TestDarwinWatcherDirectoryRenameCarriesFullSyncMetadata(t *testing.T) {
	root := t.TempDir()
	watcher, batches := newDarwinTestWatcher(t, root, nil, 50*time.Millisecond)
	watcher.Start()

	dir := filepath.Join(root, "before")
	require.NoError(t, os.Mkdir(dir, 0o700))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.Paths, dir)
	})

	renamedDir := filepath.Join(root, "after")
	require.NoError(t, os.Rename(dir, renamedDir))
	batch := waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.ContainsFunc(batch.Renames, func(rename WatchRename) bool {
			return rename.Path == renamedDir && rename.Root == root &&
				rename.ItemType == ItemIsDir
		})
	})

	assert.False(t, batch.FullSync)
	assert.Empty(t, batch.ReconcileRoots)
}

func TestDarwinWatcherExcludesRecursiveEvents(t *testing.T) {
	root := t.TempDir()
	excluded := filepath.Join(root, ".git")
	require.NoError(t, os.Mkdir(excluded, 0o700))
	watcher, batches := newDarwinTestWatcher(
		t, root, []string{".git"}, 50*time.Millisecond,
	)
	watcher.Start()

	require.NoError(t, os.WriteFile(
		filepath.Join(excluded, "config"),
		[]byte("excluded"),
		0o600,
	))
	assert.Never(t, func() bool {
		select {
		case batch := <-batches:
			t.Logf("unexpected excluded batch: %#v", batch)
			return true
		default:
			return false
		}
	}, 400*time.Millisecond, 10*time.Millisecond)
}

// TestDarwinWatcherShallowLostEventsReachConsumer covers the shallow
// watcher's lost-events full sync. That event carries no path, and the
// wrapper drops shallow events it cannot attribute to a root, so the full sync
// must bypass root matching or the events it stands in for are never
// reconciled.
func TestDarwinWatcherShallowLostEventsReachConsumer(t *testing.T) {
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	watcher, batches := newDarwinTestWatcherWithBackend(t, backend)
	watcher.Start()

	backend.shallowWatcher.consume(darwinShallowRoot{}, []fsevents.Event{{
		Flags: fseventFlagMustScanSubDirs,
	}})

	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool { return batch.FullSync })
}

// TestDarwinWatcherLostShallowEventsInvalidateRoots covers shallow-root
// recovery after FSEvents drops shallow events. A dropped event can be the
// shallow root's own removal, which normally signals loss through
// handleShallowEvent; the full-sync marker must therefore mark every active
// shallow root lost so lifecycle recovery revalidates its watch or hands it to
// polling.
func TestDarwinWatcherLostShallowEventsInvalidateRoots(t *testing.T) {
	root := t.TempDir()
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	backend.retryInitial = 5 * time.Millisecond
	backend.retryMax = 10 * time.Millisecond

	polling := make(chan PollingObligation, 4)
	batches := make(chan WatchBatch, 8)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(_ context.Context, batch WatchBatch) error {
			batches <- batch
			return nil
		}, backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{
			OnPollingRequired: func(obligation PollingObligation) error {
				polling <- obligation
				return nil
			},
			OnPollingReleased: func(string) error { return nil },
		},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Exists: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: root}},
	}}, 10)
	require.Equal(t, []RecursiveWatchResult{{Watched: 1}}, results)
	require.NoError(t, watcher.Start())

	backend.shallowWatcher.consume(darwinShallowRoot{
		logicalPath: root, nativePath: canonicalWatchRoot(root),
	}, []fsevents.Event{{Flags: fseventFlagMustScanSubDirs}})

	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool { return batch.FullSync })
	obligation := requireReceiveWithin(t, polling, 5*time.Second)
	assert.Equal(t, root, obligation.Key,
		"dropped shallow events must move the shallow root through loss recovery")
}

// A shallow full-sync marker arriving while FSEvents fallback is collecting or
// reconciling is buffered for replay, and replayed events bypass
// handleShallowEvent. The loss marking for shallow roots must therefore
// happen before fallback collection or a dropped root removal permanently
// loses coverage once fallback ends.
func TestDarwinHandleShallowFullSyncDuringFallbackMarksShallowRootsLost(t *testing.T) {
	for _, phase := range []darwinFallbackPhase{
		darwinFallbackCollecting, darwinFallbackReconciling,
	} {
		backend := &darwinWatchBackend{
			fallbackEvents: make(map[backendEvent]struct{}),
		}
		state := &darwinLogicalRoot{}
		root := t.TempDir()
		backend.roots.Store(&darwinRootSnapshot{roots: []darwinWatchRoot{{
			logicalPath: root,
			nativePath:  canonicalWatchRoot(root),
			state:       state,
		}}})
		backend.fallbackPhase.Store(uint32(phase))

		marker := backendEvent{Op: backendOpFullSync}
		_, dispatch := backend.handleShallowEvent(marker)

		assert.False(t, dispatch,
			"phase %d: the marker must be buffered for fallback replay", phase)
		assert.Contains(t, backend.fallbackEvents, marker,
			"phase %d: the marker must replay with the reconciliation", phase)
		assert.NotZero(t, state.signal.Load()&darwinRootSignalLoss,
			"phase %d: shallow roots must be marked lost before collection", phase)
	}
}

func TestDarwinWatcherExcludedDirectoryRenameIsFiltered(t *testing.T) {
	root := t.TempDir()
	backend, err := newDarwinWatchBackend([]string{".git"}, 50*time.Millisecond)
	require.NoError(t, err)
	watcher, batches := newDarwinTestWatcherWithBackend(t, backend)
	result := watcher.WatchRecursiveBudgeted(root, 1)
	require.NoError(t, result.Err)
	watcher.Start()

	backend.consumeFSEvents(darwinWatchRoot{
		logicalPath: root,
		nativePath:  canonicalWatchRoot(root),
		recursive:   true,
	}, []fsevents.Event{{
		Path:  filepath.Join(root, ".git"),
		Flags: fseventFlagItemRenamed | fseventFlagItemIsDir,
	}})

	assert.Never(t, func() bool {
		select {
		case batch := <-batches:
			t.Logf("unexpected excluded rename batch: %#v", batch)
			return true
		default:
			return false
		}
	}, 200*time.Millisecond, 10*time.Millisecond)
}

func TestDarwinWatcherStopsMappingAfterAccumulatorOverflow(t *testing.T) {
	root := t.TempDir()
	sink := &stopAfterOneDirectSink{}
	backend := &darwinWatchBackend{sink: sink}
	backend.roots.Store(&darwinRootSnapshot{roots: []darwinWatchRoot{{
		logicalPath: root,
		nativePath:  canonicalWatchRoot(root),
		recursive:   true,
	}}})
	registration := darwinWatchRoot{
		logicalPath: root,
		nativePath:  canonicalWatchRoot(root),
		recursive:   true,
	}

	backend.consumeFSEvents(registration, []fsevents.Event{
		{Path: filepath.Join(root, "first.jsonl"), Flags: fseventFlagItemModified},
		{Path: filepath.Join(root, "second.jsonl"), Flags: fseventFlagItemModified},
	})

	assert.True(t, sink.meaningful)
	assert.Equal(t, 1, sink.added)
	assert.Equal(t, filepath.Join(root, "first.jsonl"), sink.event.Path)
}

func TestDarwinWatcherMostSpecificRecursiveRootOwnsRename(t *testing.T) {
	parent := t.TempDir()
	nested := filepath.Join(parent, "nested")
	renamedPath := filepath.Join(nested, "after.jsonl")
	sink := &stopAfterOneDirectSink{}
	backend := &darwinWatchBackend{sink: sink}
	backend.roots.Store(&darwinRootSnapshot{roots: []darwinWatchRoot{
		{logicalPath: parent, nativePath: parent, recursive: true},
		{logicalPath: nested, nativePath: nested, recursive: true},
	}})
	event := []fsevents.Event{{
		Path: renamedPath, Flags: fseventFlagItemRenamed | fseventFlagItemIsFile,
	}}

	backend.consumeFSEvents(darwinWatchRoot{
		logicalPath: parent, nativePath: parent, recursive: true,
	}, event)
	assert.Zero(t, sink.added, "the parent stream must not own a nested-root rename")
	backend.consumeFSEvents(darwinWatchRoot{
		logicalPath: nested, nativePath: nested, recursive: true,
	}, event)

	assert.True(t, sink.meaningful)
	assert.Equal(t, 1, sink.added)
	assert.Equal(t, backendEvent{
		Path: renamedPath, Root: nested, Op: backendOpRename, ItemType: backendItemFile,
	}, sink.event)
}

func TestDarwinWatcherNestedShallowRootDoesNotShadowRecursiveStream(t *testing.T) {
	parent := t.TempDir()
	shallow := filepath.Join(parent, "metadata")
	changedPath := filepath.Join(shallow, "nested", "session.jsonl")
	sink := &stopAfterOneDirectSink{}
	backend := &darwinWatchBackend{sink: sink}
	backend.roots.Store(&darwinRootSnapshot{roots: []darwinWatchRoot{
		{logicalPath: parent, nativePath: parent, recursive: true},
		{logicalPath: shallow, nativePath: shallow, recursive: false},
	}})

	backend.consumeFSEvents(darwinWatchRoot{
		logicalPath: parent, nativePath: parent, recursive: true,
	}, []fsevents.Event{{
		Path: changedPath, Flags: fseventFlagItemModified | fseventFlagItemIsFile,
	}})

	assert.True(t, sink.meaningful)
	assert.Equal(t, 1, sink.added)
	assert.Equal(t, backendEvent{
		Path: changedPath, Root: parent, Op: backendOpWrite, ItemType: backendItemFile,
	}, sink.event)
}

type recordingDarwinStream struct {
	root  string
	trace *[]string
	close chan struct{}
}

func (s *recordingDarwinStream) Start() error {
	*s.trace = append(*s.trace, "stream-start:"+s.root)
	return nil
}

func (s *recordingDarwinStream) Close() error {
	*s.trace = append(*s.trace, "stream-close:"+s.root)
	if s.close != nil {
		close(s.close)
	}
	return nil
}

func newDarwinLifecycleTestWatcher(
	t *testing.T,
	trace *[]string,
) (*darwinWatchBackend, *Watcher) {
	t.Helper()
	backend, err := newDarwinWatchBackend(nil, 50*time.Millisecond)
	require.NoError(t, err)
	backend.newStream = func(root string, _ func([]fsevents.Event)) (darwinStream, error) {
		*trace = append(*trace, "stream-new:"+root)
		return &recordingDarwinStream{root: root, trace: trace}, nil
	}
	addTrace := func(path string) error {
		*trace = append(*trace, "shallow-add:"+path)
		return nil
	}
	removeTrace := func(path string) error {
		*trace = append(*trace, "shallow-remove:"+path)
		return nil
	}
	backend.addShallow = addTrace
	backend.removeShallow = removeTrace
	backend.addAncestor = addTrace
	backend.removeAncestor = removeTrace
	watcher, err := newWatcherWithBackend(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	return backend, watcher
}

func setDarwinLifecycleRunningForTest(t *testing.T, backend *darwinWatchBackend) {
	t.Helper()
	backend.lifecycle = darwinBackendRunning
	t.Cleanup(func() {
		backend.mu.Lock()
		if backend.lifecycle == darwinBackendRunning {
			backend.lifecycle = darwinBackendNew
		}
		backend.mu.Unlock()
	})
}

func TestDarwinWatcherHybridRegistrationUsesShallowAndPendingCoverage(t *testing.T) {
	ancestor := t.TempDir()
	shallow := filepath.Join(ancestor, "metadata")
	require.NoError(t, os.Mkdir(shallow, 0o700))
	pendingOne := filepath.Join(ancestor, "state", "sessions")
	pendingTwo := filepath.Join(ancestor, "state", "archive")
	var trace []string
	_, watcher := newDarwinLifecycleTestWatcher(t, &trace)

	results := watcher.RegisterRoots([]WatchRoot{
		{Path: shallow, Exists: true, Scopes: []WatchScope{{Agent: "agent-a", SyncDir: shallow}}},
		{Path: pendingOne, Recursive: true, Scopes: []WatchScope{{Agent: "agent-b", SyncDir: ancestor}}},
		{Path: pendingTwo, Recursive: true, Scopes: []WatchScope{{Agent: "agent-c", SyncDir: ancestor}}},
	}, 10)

	require.Len(t, results, 3)
	assert.Equal(t, []RecursiveWatchResult{
		{Watched: 1},
		{Watched: 1, MissingRootLifecycleOwned: true},
		{Watched: 1, MissingRootLifecycleOwned: true},
	}, results)
	assert.Equal(t, []string{
		"shallow-add:" + shallow,
		"shallow-add:" + ancestor,
	}, trace, "an explicit shallow root never creates a stream and pending roots share one ancestor watch")
}

func TestDarwinWatcherRegistrationOwnsRootCreatedAfterCollection(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "sessions")
	plan := WatchRoot{
		Path: root, Recursive: true, Exists: false,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: parent}},
	}
	var trace []string
	backend, watcher := newDarwinLifecycleTestWatcher(t, &trace)
	backend.pathIsDirectory = func(path string) bool {
		require.Equal(t, root, path)
		require.NoError(t, os.Mkdir(root, 0o700))
		return false
	}

	results := watcher.RegisterRoots([]WatchRoot{plan}, 10)

	require.Equal(t, []RecursiveWatchResult{{
		Watched: 1, MissingRootLifecycleOwned: true,
	}}, results)
	assert.Equal(t, []string{"shallow-add:" + root}, trace,
		"the registration race initially acquires bounded root coverage")

	backend.processLifecycle(true)

	assert.Equal(t, []string{
		"shallow-add:" + root,
		"stream-new:" + root,
		"shallow-remove:" + root,
	}, trace, "the pending root advances into native recursive coverage")
}

func TestDarwinWatcherPendingRootRetriesWithoutNativeShallowDelivery(t *testing.T) {
	ancestor := t.TempDir()
	root := filepath.Join(ancestor, "state", "sessions")
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	backend.retryInitial = 10 * time.Millisecond
	backend.retryMax = 20 * time.Millisecond
	backend.startShallow = func() error { return nil }
	// Without a parent-folder observer, only the lifecycle retry can notice
	// the root appearing.
	backend.addAncestor = func(string) error { return nil }
	backend.removeAncestor = func(string) error { return nil }

	missingChecked := make(chan struct{})
	var missingCheckedOnce sync.Once
	backend.lstat = func(path string) (os.FileInfo, error) {
		info, statErr := os.Lstat(path)
		if path == root && os.IsNotExist(statErr) {
			missingCheckedOnce.Do(func() { close(missingChecked) })
		}
		return info, statErr
	}
	streamStarted := make(chan struct{})
	var streamStartedOnce sync.Once
	backend.newStream = func(string, func([]fsevents.Event)) (darwinStream, error) {
		return &handoffRecordingStream{start: func() {
			streamStartedOnce.Do(func() { close(streamStarted) })
		}}, nil
	}

	watcher, batches := newDarwinTestWatcherWithBackend(t, backend)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	require.Equal(t, []RecursiveWatchResult{{
		Watched: 1, MissingRootLifecycleOwned: true,
	}}, results)
	watcher.Start()

	requireReceiveWithin(t, missingChecked, time.Second)
	require.NoError(t, os.MkdirAll(root, 0o700))
	requireReceiveWithin(t, streamStarted, time.Second)
	batch := waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.ReconcileRoots, ancestor)
	})
	assert.Equal(t, []string{ancestor}, batch.ReconcileRoots)
	assert.Empty(t, batch.Paths)
}

func TestDarwinWatcherMissingIntermediateMovesCoverageBeforeRelease(t *testing.T) {
	ancestor := t.TempDir()
	intermediate := filepath.Join(ancestor, "state")
	root := filepath.Join(intermediate, "sessions")
	var trace []string
	backend, watcher := newDarwinLifecycleTestWatcher(t, &trace)
	watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	require.NoError(t, os.Mkdir(intermediate, 0o700))

	_, dispatch := backend.handleShallowEvent(backendEvent{
		Path: intermediate, Op: backendOpCreate, ItemType: backendItemDirectory,
	})
	backend.processLifecycleSignals()

	assert.False(t, dispatch, "the scope reconciliation subsumes the component creation")
	assert.Equal(t, []string{
		"shallow-add:" + ancestor,
		"shallow-add:" + intermediate,
		"shallow-remove:" + ancestor,
	}, trace)
	batch, ok := watcher.eventSink.Take(watcher.agentsForRoot)
	require.True(t, ok)
	assert.Equal(t, []string{ancestor}, batch.ReconcileRoots)
}

func TestDarwinWatcherMissingRootStartsCollectingBeforeReleaseAndDispatch(t *testing.T) {
	ancestor := t.TempDir()
	root := filepath.Join(ancestor, "sessions")
	var trace []string
	backend, watcher := newDarwinLifecycleTestWatcher(t, &trace)
	watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	setDarwinLifecycleRunningForTest(t, backend)
	require.NoError(t, os.Mkdir(root, 0o700))

	_, dispatch := backend.handleShallowEvent(backendEvent{
		Path: root, Op: backendOpCreate, ItemType: backendItemDirectory,
	})
	backend.processLifecycleSignals()

	assert.False(t, dispatch)
	assert.Equal(t, []string{
		"shallow-add:" + ancestor,
		"stream-new:" + root,
		"stream-start:" + root,
		"shallow-remove:" + ancestor,
	}, trace)
	batch, ok := watcher.eventSink.Take(watcher.agentsForRoot)
	require.True(t, ok)
	assert.Equal(t, []string{ancestor}, batch.ReconcileRoots,
		"transition reconciliation uses configured syncDir scopes")
}

func TestDarwinWatcherHybridHandoffRetainsWritesFromEveryPhase(t *testing.T) {
	ancestor := t.TempDir()
	root := filepath.Join(ancestor, "sessions")
	paths := []string{
		filepath.Join(root, "during-stream-start.jsonl"),
		filepath.Join(root, "during-shallow-release.jsonl"),
	}
	var trace []string
	backend, watcher := newDarwinLifecycleTestWatcher(t, &trace)
	backend.newStream = func(streamRoot string, _ func([]fsevents.Event)) (darwinStream, error) {
		return &handoffRecordingStream{
			start: func() {
				watcher.eventSink.Add(backendEvent{Path: paths[0], Root: streamRoot, Op: backendOpWrite})
			},
		}, nil
	}
	backend.removeAncestor = func(path string) error {
		watcher.eventSink.Add(backendEvent{Path: paths[1], Root: root, Op: backendOpWrite})
		return nil
	}
	watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	setDarwinLifecycleRunningForTest(t, backend)
	require.NoError(t, os.Mkdir(root, 0o700))

	backend.handleShallowEvent(backendEvent{
		Path: root, Op: backendOpCreate, ItemType: backendItemDirectory,
	})
	backend.processLifecycleSignals()

	batch, ok := watcher.eventSink.Take(watcher.agentsForRoot)
	require.True(t, ok)
	assert.ElementsMatch(t, paths, batch.Paths)
	assert.Equal(t, []string{ancestor}, batch.ReconcileRoots)
}

func TestDarwinWatcherRootDeletionBridgesTeardownAndRecreation(t *testing.T) {
	ancestor := t.TempDir()
	root := filepath.Join(ancestor, "sessions")
	require.NoError(t, os.Mkdir(root, 0o700))
	var trace []string
	backend, watcher := newDarwinLifecycleTestWatcher(t, &trace)
	closed := make(chan struct{}, 1)
	streamCount := 0
	backend.newStream = func(streamRoot string, _ func([]fsevents.Event)) (darwinStream, error) {
		streamCount++
		return &recordingDarwinStream{
			root: streamRoot, trace: &trace,
			close: func() chan struct{} {
				if streamCount == 1 {
					return closed
				}
				return nil
			}(),
		}, nil
	}
	watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	setDarwinLifecycleRunningForTest(t, backend)
	require.NoError(t, os.Remove(root))

	backend.consumeFSEvents(darwinWatchRoot{
		logicalPath: root, nativePath: root, recursive: true,
		state: backend.logical[root], generation: backend.logical[root].generation,
	}, []fsevents.Event{{Path: root, Flags: fseventFlagRootChanged}})
	backend.processLifecycleSignals()
	requireReceiveWithin(t, closed, time.Second)
	requireReceiveWithin(t, watcher.eventSink.wake, time.Second)
	batch, ok := watcher.eventSink.Take(watcher.agentsForRoot)
	require.True(t, ok)
	assert.Equal(t, []string{ancestor}, batch.ReconcileRoots,
		"authoritative scope reconciliation tombstones the disappeared root")
	assert.Less(t, slices.Index(trace, "shallow-add:"+ancestor),
		slices.Index(trace, "stream-close:"+root),
		"ancestor coverage must precede stream teardown")
	for _, token := range batch.lifecycleTokens {
		token.gate.acknowledgeLifecycle(token.generation)
	}
	backend.processLifecycleSignals()

	require.NoError(t, os.Mkdir(root, 0o700))
	backend.handleShallowEvent(backendEvent{
		Path: root, Op: backendOpCreate, ItemType: backendItemDirectory,
	})
	backend.processLifecycleSignals()
	batch, ok = watcher.eventSink.Take(watcher.agentsForRoot)
	require.True(t, ok)
	assert.Equal(t, []string{ancestor}, batch.ReconcileRoots)
	start := slices.Index(trace, "stream-start:"+root)
	release := slices.Index(trace, "shallow-remove:"+ancestor)
	assert.NotEqual(t, -1, start)
	assert.NotEqual(t, -1, release)
	assert.Less(t, start, release, "recreated root collects before ancestor release")
}

func TestDarwinWatcherRootDeletionHandoffRetainsWrites(t *testing.T) {
	ancestor := t.TempDir()
	root := filepath.Join(ancestor, "sessions")
	require.NoError(t, os.Mkdir(root, 0o700))
	var trace []string
	backend, watcher := newDarwinLifecycleTestWatcher(t, &trace)
	paths := []string{
		filepath.Join(root, "during-ancestor-add.jsonl"),
		filepath.Join(root, "during-stream-close.jsonl"),
	}
	backend.addAncestor = func(string) error {
		watcher.eventSink.Add(backendEvent{Path: paths[0], Root: root, Op: backendOpWrite})
		return nil
	}
	closed := make(chan struct{})
	backend.newStream = func(string, func([]fsevents.Event)) (darwinStream, error) {
		return &handoffRecordingStream{close: func() {
			watcher.eventSink.Add(backendEvent{Path: paths[1], Root: root, Op: backendOpWrite})
			close(closed)
		}}, nil
	}
	watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	setDarwinLifecycleRunningForTest(t, backend)
	require.NoError(t, os.Remove(root))

	backend.consumeFSEvents(darwinWatchRoot{
		logicalPath: root, nativePath: root, recursive: true,
		state: backend.logical[root], generation: backend.logical[root].generation,
	}, []fsevents.Event{{Path: root, Flags: fseventFlagRootChanged}})
	backend.processLifecycleSignals()
	requireReceiveWithin(t, closed, time.Second)
	requireReceiveWithin(t, watcher.eventSink.wake, time.Second)
	batch, ok := watcher.eventSink.Take(watcher.agentsForRoot)
	require.True(t, ok)
	assert.ElementsMatch(t, paths, batch.Paths)
	assert.Equal(t, []string{ancestor}, batch.ReconcileRoots)
}

func TestDarwinWatcherRetiredStreamLossDoesNotRetireReplacement(t *testing.T) {
	ancestor := t.TempDir()
	root := filepath.Join(ancestor, "sessions")
	require.NoError(t, os.Mkdir(root, 0o700))
	var trace []string
	backend, watcher := newDarwinLifecycleTestWatcher(t, &trace)
	var nativeSink func([]fsevents.Event)
	closed := 0
	backend.newStream = func(_ string, sink func([]fsevents.Event)) (darwinStream, error) {
		nativeSink = sink
		return &handoffRecordingStream{close: func() {
			closed++
			sink([]fsevents.Event{{Path: root, Flags: fseventFlagRootChanged}})
		}}, nil
	}
	watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	setDarwinLifecycleRunningForTest(t, backend)

	nativeSink([]fsevents.Event{{Path: root, Flags: fseventFlagRootChanged}})
	backend.processLifecycleSignals()
	for range 2 {
		batch, ok := watcher.eventSink.Take(watcher.agentsForRoot)
		require.True(t, ok)
		require.Equal(t, []string{ancestor}, batch.ReconcileRoots)
		for _, token := range batch.lifecycleTokens {
			token.gate.acknowledgeLifecycle(token.generation)
		}
		backend.processLifecycleSignals()
	}

	backend.processLifecycleSignals()
	state := backend.logical[root]
	assert.Equal(t, 1, closed, "a callback drained during closure belongs to the retired stream")
	assert.True(t, state.active)
	assert.True(t, state.open.Load())
	assert.Equal(t, darwinRootOpen, state.phase)
	path := filepath.Join(root, "after-replacement.jsonl")
	nativeSink([]fsevents.Event{{Path: path, Flags: fseventFlagItemModified | fseventFlagItemIsFile}})
	batch, ok := watcher.eventSink.Take(watcher.agentsForRoot)
	require.True(t, ok)
	assert.Equal(t, []string{path}, batch.Paths)
}

func TestDarwinWatcherMissingRootAncestorAvoidsPerEntryWatch(t *testing.T) {
	ancestor := t.TempDir()
	for i := range 50 {
		require.NoError(t, os.WriteFile(
			filepath.Join(ancestor, fmt.Sprintf("entry%02d", i)), nil, 0o644))
	}
	backend, err := newDarwinWatchBackend(nil, time.Millisecond)
	require.NoError(t, err)
	defer backend.Stop()
	var shallowAdds []string
	backend.addShallow = func(path string) error {
		shallowAdds = append(shallowAdds, path)
		return nil
	}
	missing := filepath.Join(ancestor, "provider", "sessions")
	results := backend.RegisterRoots(
		[]WatchRoot{{Path: missing, Recursive: true}}, 1024)
	require.Len(t, results, 1)
	require.True(t, results[0].MissingRootLifecycleOwned)
	assert.Empty(t, shallowAdds,
		"ancestor coverage must not use a shallow watch")
	require.NotNil(t, backend.vnode)
	assert.Equal(t, 1, backend.vnode.watchedCount())
}

func TestDarwinWatcherMissingRootRealCreationDeletionRecreation(t *testing.T) {
	ancestor := t.TempDir()
	intermediate := filepath.Join(ancestor, "state")
	root := filepath.Join(intermediate, "sessions")
	second := filepath.Join(root, "second.jsonl")
	var secondObserved atomic.Bool
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	watcher, batches := newDarwinTestWatcherWithBackend(t, backend)
	onChange := watcher.onChange
	watcher.onChange = func(ctx context.Context, batch WatchBatch) error {
		if slices.Contains(batch.Paths, second) || batch.FullSync ||
			slices.Contains(batch.ReconcileRoots, ancestor) {
			if _, err := os.Stat(second); err == nil {
				secondObserved.Store(true)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return onChange(ctx, batch)
	}
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	require.Equal(t, RecursiveWatchResult{
		Watched: 1, MissingRootLifecycleOwned: true,
	}, results[0])
	rootOpen := backend.logical[root].open.Load
	watcher.Start()

	require.NoError(t, os.Mkdir(intermediate, 0o700))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.ReconcileRoots, ancestor)
	})
	require.NoError(t, os.Mkdir(root, 0o700))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.ReconcileRoots, ancestor)
	})
	require.Eventually(t, rootOpen, 5*time.Second, time.Millisecond)
	first := filepath.Join(root, "first.jsonl")
	require.NoError(t, os.WriteFile(first, []byte("first"), 0o600))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.Paths, first)
	})

	require.NoError(t, os.RemoveAll(root))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.ReconcileRoots, ancestor)
	})
	require.NoError(t, os.Mkdir(root, 0o700))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.ReconcileRoots, ancestor)
	})
	require.Eventually(t, rootOpen, 5*time.Second, time.Millisecond)
	require.NoError(t, os.WriteFile(second, []byte("second"), 0o600))
	// Delayed removal events can retire the replacement stream after it opens.
	// Its reconciliation must observe the file even when no path event survives.
	require.Eventually(t, secondObserved.Load, 5*time.Second, time.Millisecond)
}

func TestDarwinWatcherRootShallowRenameAndRecreation(t *testing.T) {
	ancestor := t.TempDir()
	root := filepath.Join(ancestor, "metadata")
	require.NoError(t, os.Mkdir(root, 0o700))
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	watcher, batches := newDarwinTestWatcherWithBackend(t, backend)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path:   root,
		Exists: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	require.Equal(t, RecursiveWatchResult{Watched: 1}, results[0])
	watcher.Start()

	first := filepath.Join(root, "first.json")
	require.NoError(t, os.WriteFile(first, []byte("first"), 0o600))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.Paths, first)
	})
	moved := filepath.Join(ancestor, "metadata-old")
	require.NoError(t, os.Rename(root, moved))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.ReconcileRoots, ancestor)
	})
	require.NoError(t, os.Mkdir(root, 0o700))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.ReconcileRoots, ancestor)
	})
	second := filepath.Join(root, "second.json")
	require.NoError(t, os.WriteFile(second, []byte("second"), 0o600))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.Paths, second)
	})
}

func TestDarwinWatcherNativeCallbackDoesNotWaitForLifecycleLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	require.NoError(t, os.Mkdir(root, 0o700))
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	var nativeSink func([]fsevents.Event)
	backend.newStream = func(_ string, sink func([]fsevents.Event)) (darwinStream, error) {
		nativeSink = sink
		return &handoffRecordingStream{}, nil
	}
	watcher, _ := newDarwinTestWatcherWithBackend(t, backend)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: filepath.Dir(root)}},
	}}, 10)
	require.Equal(t, RecursiveWatchResult{Watched: 1}, results[0])
	require.NoError(t, os.Remove(root))
	var lifecycleCalls atomic.Int32
	countCall := func(string) error {
		lifecycleCalls.Add(1)
		return nil
	}
	backend.addShallow = countCall
	backend.removeShallow = countCall
	backend.addAncestor = countCall
	backend.removeAncestor = countCall
	backend.newStream = func(string, func([]fsevents.Event)) (darwinStream, error) {
		lifecycleCalls.Add(1)
		return &handoffRecordingStream{}, nil
	}

	backend.mu.Lock()
	returned := make(chan struct{})
	go func() {
		nativeSink([]fsevents.Event{{Path: root, Flags: fseventFlagRootChanged}})
		close(returned)
	}()

	select {
	case <-returned:
		assert.Zero(t, lifecycleCalls.Load(),
			"native delivery must only signal deferred lifecycle work")
		backend.mu.Unlock()
	case <-time.After(watcherTestTimeout):
		backend.mu.Unlock()
		require.FailNow(t, "native callback waited for lifecycle ownership")
	}
}

func TestDarwinWatcherHybridTransitionDoesNotSuppressOtherShallowRootEvent(t *testing.T) {
	shallow := t.TempDir()
	pending := filepath.Join(shallow, "sessions")
	var trace []string
	backend, watcher := newDarwinLifecycleTestWatcher(t, &trace)
	results := watcher.RegisterRoots([]WatchRoot{
		{Path: shallow, Exists: true, Scopes: []WatchScope{{Agent: "agent-a", SyncDir: shallow}}},
		{Path: pending, Recursive: true, Scopes: []WatchScope{{Agent: "agent-b", SyncDir: shallow}}},
	}, 10)
	require.Equal(t, []RecursiveWatchResult{
		{Watched: 1},
		{Watched: 1, MissingRootLifecycleOwned: true},
	}, results)
	require.NoError(t, os.Mkdir(pending, 0o700))

	event, dispatch := backend.handleShallowEvent(backendEvent{
		Path: pending, Op: backendOpCreate, ItemType: backendItemDirectory,
	})

	assert.True(t, dispatch, "root B lifecycle work must not consume root A's ordinary event")
	assert.Equal(t, shallow, event.Root)
}

func TestDarwinWatcherCollectingGateWaitsForSuccessfulReconciliation(t *testing.T) {
	ancestor := t.TempDir()
	root := filepath.Join(ancestor, "sessions")
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	var nativeSink func([]fsevents.Event)
	backend.newStream = func(_ string, sink func([]fsevents.Event)) (darwinStream, error) {
		nativeSink = sink
		return &handoffRecordingStream{}, nil
	}
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSuccess := make(chan struct{})
	unblockFirst := sync.OnceFunc(func() { close(releaseFirst) })
	unblockSuccess := sync.OnceFunc(func() { close(releaseSuccess) })
	successReturned := make(chan struct{})
	calls := make(chan WatchBatch, 8)
	var attempts atomic.Int32
	watcher, err := newWatcherWithBackend(
		0, 0,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			switch attempts.Add(1) {
			case 1:
				close(firstEntered)
				<-releaseFirst
				return errors.New("reconciliation unavailable")
			case 2:
				<-releaseSuccess
				close(successReturned)
			}
			return nil
		},
		backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	// Release a parked callback before Stop waits for it.
	t.Cleanup(unblockSuccess)
	t.Cleanup(unblockFirst)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	require.Equal(t, RecursiveWatchResult{
		Watched: 1, MissingRootLifecycleOwned: true,
	}, results[0])
	watcher.Start()
	require.NoError(t, os.Mkdir(root, 0o700))
	backend.handleShallowEvent(backendEvent{
		Path: root, Op: backendOpCreate, ItemType: backendItemDirectory,
	})
	requireReceiveWithin(t, firstEntered, time.Second)

	path := filepath.Join(root, "during-collecting.jsonl")
	nativeSink([]fsevents.Event{{Path: path, Flags: fseventFlagItemModified | fseventFlagItemIsFile}})
	unblockFirst()
	_ = requireReceiveWithin(t, calls, time.Second)
	second := requireReceiveWithin(t, calls, time.Second)
	assert.Empty(t, second.Paths,
		"ordinary paths must remain gated while exact reconciliation is retrying")
	assert.Equal(t, []string{ancestor}, second.ReconcileRoots)
	unblockSuccess()
	requireReceiveWithin(t, successReturned, time.Second)
}

func TestDarwinWatcherRootChangedABAReplacesRecursiveCoverageAfterTombstone(t *testing.T) {
	ancestor := t.TempDir()
	root := filepath.Join(ancestor, "sessions")
	require.NoError(t, os.Mkdir(root, 0o700))
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	sinks := make(chan func([]fsevents.Event), 2)
	closed := make(chan struct{}, 1)
	backend.newStream = func(_ string, sink func([]fsevents.Event)) (darwinStream, error) {
		sinks <- sink
		return &handoffRecordingStream{close: func() { closed <- struct{}{} }}, nil
	}
	watcher, batches := newDarwinTestWatcherWithBackend(t, backend)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	require.Equal(t, RecursiveWatchResult{Watched: 1}, results[0])
	initialSink := requireReceiveWithin(t, sinks, time.Second)
	watcher.Start()

	initialSink([]fsevents.Event{{Path: root, Flags: fseventFlagRootChanged}})
	requireReceiveWithin(t, closed, time.Second)
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.ReconcileRoots, ancestor)
	})
	replacementSink := requireReceiveWithin(t, sinks, time.Second)
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.ReconcileRoots, ancestor)
	})
	require.Eventually(t, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return backend.logical[root].open.Load()
	}, 5*time.Second, time.Millisecond)
	path := filepath.Join(root, "after-aba.jsonl")
	replacementSink([]fsevents.Event{{Path: path, Flags: fseventFlagItemModified | fseventFlagItemIsFile}})
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.Paths, path)
	})
}

func TestDarwinWatcherShallowRenameABAReplacesCoverage(t *testing.T) {
	ancestor := t.TempDir()
	root := filepath.Join(ancestor, "metadata")
	require.NoError(t, os.Mkdir(root, 0o700))
	var trace []string
	backend, watcher := newDarwinLifecycleTestWatcher(t, &trace)
	batches := make(chan WatchBatch, 8)
	watcher.onChange = func(_ context.Context, batch WatchBatch) error {
		batches <- batch
		return nil
	}
	results := watcher.RegisterRoots([]WatchRoot{{
		Path:   root,
		Exists: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	require.Equal(t, RecursiveWatchResult{Watched: 1}, results[0])
	initialGeneration := backend.logical[root].generation
	watcher.Start()

	backend.handleShallowEvent(backendEvent{
		Path: root, Root: root, Op: backendOpRename, ItemType: backendItemDirectory,
	})

	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.ReconcileRoots, ancestor)
	})
	require.Eventually(t, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		state := backend.logical[root]
		return state != nil && state.generation > initialGeneration &&
			state.ancestor == root && state.active && state.open.Load()
	}, 5*time.Second, time.Millisecond)
	watcher.Stop()
	assert.Contains(t, trace, "shallow-remove:"+root)
	assert.Contains(t, trace, "shallow-add:"+ancestor)
	assert.Contains(t, trace, "shallow-add:"+root)
}

type retryDarwinStream struct {
	startErr error
	sink     func([]fsevents.Event)
}

func (s *retryDarwinStream) Start() error { return s.startErr }
func (s *retryDarwinStream) Close() error { return nil }

func TestDarwinWatcherRuntimeStreamAcquisitionFailureFallsBack(t *testing.T) {
	for _, failure := range []string{"new", "start"} {
		t.Run(failure, func(t *testing.T) {
			ancestor := t.TempDir()
			root := filepath.Join(ancestor, "sessions")
			backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
			require.NoError(t, err)
			backend.retryInitial = time.Millisecond
			backend.retryMax = 5 * time.Millisecond
			var attempts atomic.Int32
			var latestSink atomic.Pointer[func([]fsevents.Event)]
			backend.newStream = func(_ string, sink func([]fsevents.Event)) (darwinStream, error) {
				attempt := attempts.Add(1)
				if failure == "new" && attempt == 1 {
					return nil, errors.New("stream unavailable")
				}
				latestSink.Store(&sink)
				stream := &retryDarwinStream{sink: sink}
				if failure == "start" && attempt == 1 {
					stream.startErr = errors.New("start unavailable")
				}
				return stream, nil
			}
			batches := make(chan WatchBatch, 8)
			watcher, err := newWatcherWithBackendOptions(
				0, 0, func(_ context.Context, batch WatchBatch) error {
					batches <- batch
					return nil
				}, backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
				WatcherOptions{OnCoverageDegraded: func([]string) error { return nil }},
			)
			require.NoError(t, err)
			t.Cleanup(watcher.Stop)
			results := watcher.RegisterRoots([]WatchRoot{{
				Path: root, Recursive: true,
				Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
			}}, 10)
			require.Equal(t, RecursiveWatchResult{
				Watched: 1, MissingRootLifecycleOwned: true,
			}, results[0])
			require.NoError(t, watcher.Start())
			require.NoError(t, os.Mkdir(root, 0o700))
			backend.signalLifecycle()

			waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
				return batch.FullSync
			})
			require.Eventually(t, func() bool {
				return backend.fallbackPhaseValue() == darwinFallbackInactive
			}, time.Second, time.Millisecond)
			assert.Equal(t, int32(2), attempts.Load(), "native coverage is retried once")
			require.Eventually(t, watcher.eventSink.Empty, time.Second, time.Millisecond,
				"fallback handoff markers must drain before steady-state delivery")
			path := filepath.Join(root, "after-retry.jsonl")
			sink := latestSink.Load()
			require.NotNil(t, sink)
			(*sink)([]fsevents.Event{{
				Path: path, Flags: fseventFlagItemModified | fseventFlagItemIsFile,
			}})
			waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
				return slices.Contains(batch.Paths, path)
			})
		})
	}
}

func TestDarwinWatcherFallbackRecoversNativeStreamsAndReleasesPolling(t *testing.T) {
	root := t.TempDir()
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	backend.retryInitial = 5 * time.Millisecond
	backend.retryMax = 10 * time.Millisecond

	var sinksMu sync.Mutex
	var sinks []func([]fsevents.Event)
	backend.newStream = func(_ string, sink func([]fsevents.Event)) (darwinStream, error) {
		sinksMu.Lock()
		sinks = append(sinks, sink)
		sinksMu.Unlock()
		return &handoffRecordingStream{}, nil
	}
	polling := make(chan PollingObligation, 2)
	released := make(chan string, 1)
	batches := make(chan WatchBatch, 8)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(_ context.Context, batch WatchBatch) error {
			batches <- batch
			return nil
		}, backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{
			OnCoverageDegraded: func(roots []string) error {
				scopes := make([]PollingScope, len(roots))
				for i, r := range roots {
					scopes[i] = PollingScope{Root: r}
				}
				polling <- PollingObligation{Key: "watcher-fallback", Scopes: scopes}
				return nil
			},
			OnPollingRequired: func(obligation PollingObligation) error {
				polling <- obligation
				return nil
			},
			OnPollingReleased: func(key string) error {
				released <- key
				return nil
			},
		},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: root}},
	}}, 10)
	require.Equal(t, []RecursiveWatchResult{{Watched: 1}}, results)
	require.NoError(t, watcher.Start())

	sinksMu.Lock()
	require.Len(t, sinks, 1)
	sinksMu.Unlock()
	backend.requestFallback(darwinFallbackNativeDrop)

	assert.Equal(t, PollingObligation{
		Key:    darwinFallbackPollingObligationKey(root),
		Scopes: []PollingScope{{Agent: "agent-a", Root: root}},
		Probe:  root,
	}, requireReceiveWithin(t, polling, time.Second))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool { return batch.FullSync })
	assert.Equal(t, darwinFallbackPollingObligationKey(root),
		requireReceiveWithin(t, released, time.Second))
	require.Eventually(t, func() bool {
		return backend.fallbackPhaseValue() == darwinFallbackInactive &&
			darwinFallbackReason(backend.fallbackReason.Load()) == darwinFallbackReasonNone
	}, time.Second, time.Millisecond)
	sinksMu.Lock()
	require.Len(t, sinks, 2)
	recoveredSink := sinks[1]
	sinksMu.Unlock()
	changed := filepath.Join(root, "after-recovery.jsonl")
	recoveredSink([]fsevents.Event{{
		Path: changed, Flags: fseventFlagItemModified | fseventFlagItemIsFile,
	}})
	batch := waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.Paths, changed)
	})
	assert.Contains(t, batch.Paths, changed)
}

func TestDarwinWatcherFallbackKeepsPollingUntilNativeRetrySucceeds(t *testing.T) {
	root := t.TempDir()
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	backend.retryInitial = 40 * time.Millisecond
	backend.retryMax = 40 * time.Millisecond

	failedRecovery := make(chan struct{})
	var attempts atomic.Int32
	backend.newStream = func(_ string, _ func([]fsevents.Event)) (darwinStream, error) {
		switch attempts.Add(1) {
		case 2:
			close(failedRecovery)
			return nil, errors.New("temporary stream create failure")
		default:
			return &handoffRecordingStream{}, nil
		}
	}
	polling := make(chan PollingObligation, 1)
	released := make(chan string, 1)
	batches := make(chan WatchBatch, 8)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(_ context.Context, batch WatchBatch) error {
			batches <- batch
			return nil
		}, backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{
			OnPollingRequired: func(obligation PollingObligation) error {
				polling <- obligation
				return nil
			},
			OnPollingReleased: func(key string) error {
				released <- key
				return nil
			},
		},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{SyncDir: root}},
	}}, 10)
	require.Equal(t, []RecursiveWatchResult{{Watched: 1}}, results)
	require.NoError(t, watcher.Start())

	backend.requestFallback(darwinFallbackNativeDrop)
	assert.Equal(t, PollingObligation{
		Key:    darwinFallbackPollingObligationKey(root),
		Scopes: []PollingScope{{Root: root}},
		Probe:  root,
	}, requireReceiveWithin(t, polling, time.Second))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool { return batch.FullSync })
	requireReceiveWithin(t, failedRecovery, time.Second)
	select {
	case key := <-released:
		require.Fail(t, "polling released after failed native recovery", key)
	default:
	}

	assert.Equal(t, darwinFallbackPollingObligationKey(root),
		requireReceiveWithin(t, released, time.Second))
	require.Eventually(t, func() bool {
		return backend.fallbackPhaseValue() == darwinFallbackInactive
	}, time.Second, time.Millisecond)
	assert.Equal(t, int32(3), attempts.Load())
}

// TestDarwinWatcherStopDuringRecoveryReleaseDoesNotPanic stops the watcher
// while the recovery handoff is inside its polling-release callback, which
// runs with the backend mutex released. The lifecycle goroutine reports the
// release failure on the shared error channel after the callback returns, so
// the shallow forwarder must not close that channel until the lifecycle
// goroutine has exited; a premature close panics with a send on a closed
// channel.
func TestDarwinWatcherStopDuringRecoveryReleaseDoesNotPanic(t *testing.T) {
	root := t.TempDir()
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	backend.retryInitial = 5 * time.Millisecond
	backend.retryMax = 10 * time.Millisecond

	backend.newStream = func(_ string, _ func([]fsevents.Event)) (darwinStream, error) {
		return &handoffRecordingStream{}, nil
	}
	releaseEntered := make(chan struct{})
	var releaseCalls atomic.Int32
	batches := make(chan WatchBatch, 8)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(_ context.Context, batch WatchBatch) error {
			batches <- batch
			return nil
		}, backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{
			OnPollingRequired: func(PollingObligation) error { return nil },
			OnPollingReleased: func(string) error {
				if releaseCalls.Add(1) > 1 {
					return nil
				}
				close(releaseEntered)
				// Wait until shutdown has stopped the shallow event source.
				<-backend.shallowWatcher.done
				return errors.New("release failed during shutdown")
			},
		},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: root}},
	}}, 10)
	require.Equal(t, []RecursiveWatchResult{{Watched: 1}}, results)
	require.NoError(t, watcher.Start())

	backend.requestFallback(darwinFallbackNativeDrop)
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool { return batch.FullSync })
	requireReceiveWithin(t, releaseEntered, 30*time.Second)

	stopDone := make(chan struct{})
	go func() {
		watcher.Stop()
		close(stopDone)
	}()
	requireReceiveWithin(t, stopDone, 30*time.Second)
}

func TestDarwinWatcherFallbackTransfersMissingRootPollingBeforeRecovery(t *testing.T) {
	parent := t.TempDir()
	present := filepath.Join(parent, "present")
	missing := filepath.Join(parent, "missing")
	require.NoError(t, os.Mkdir(present, 0o700))
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	backend.retryInitial = 5 * time.Millisecond
	backend.retryMax = 10 * time.Millisecond
	backend.newStream = func(string, func([]fsevents.Event)) (darwinStream, error) {
		return &handoffRecordingStream{}, nil
	}

	polling := make(chan PollingObligation, 4)
	released := make(chan string, 4)
	batches := make(chan WatchBatch, 8)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(_ context.Context, batch WatchBatch) error {
			batches <- batch
			return nil
		}, backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{
			OnPollingRequired: func(obligation PollingObligation) error {
				polling <- obligation
				return nil
			},
			OnPollingReleased: func(key string) error {
				released <- key
				return nil
			},
		},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	results := watcher.RegisterRoots([]WatchRoot{
		{
			Path: present, Recursive: true, Exists: true,
			Scopes: []WatchScope{{SyncDir: present}},
		},
		{
			Path: missing, Recursive: true,
			Scopes: []WatchScope{{SyncDir: missing}},
		},
	}, 10)
	require.Equal(t, []RecursiveWatchResult{
		{Watched: 1},
		{Watched: 1, MissingRootLifecycleOwned: true},
	}, results)
	require.NoError(t, watcher.Start())

	backend.requestFallback(darwinFallbackNativeDrop)
	assert.Equal(t, PollingObligation{
		Key:    darwinFallbackPollingObligationKey(missing),
		Scopes: []PollingScope{{Root: missing}},
		Probe:  missing,
	}, requireReceiveWithin(t, polling, time.Second),
		"each watch plan owns its own fallback obligation probed on its path")
	assert.Equal(t, PollingObligation{
		Key:    darwinFallbackPollingObligationKey(present),
		Scopes: []PollingScope{{Root: present}},
		Probe:  present,
	}, requireReceiveWithin(t, polling, time.Second))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool { return batch.FullSync })
	assert.Equal(t, PollingObligation{Key: missing, Scopes: []PollingScope{{Root: missing}}, Probe: missing},
		requireReceiveWithin(t, polling, time.Second),
		"the skipped root must own polling before generic fallback polling is released")
	assert.Equal(t, darwinFallbackPollingObligationKey(missing),
		requireReceiveWithin(t, released, time.Second))
	assert.Equal(t, darwinFallbackPollingObligationKey(present),
		requireReceiveWithin(t, released, time.Second))
	require.Eventually(t, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		state := backend.logical[missing]
		return backend.fallbackPhaseValue() == darwinFallbackInactive &&
			state != nil && state.phase == darwinRootPending && state.ancestor == parent
	}, time.Second, time.Millisecond)

	require.NoError(t, os.Mkdir(missing, 0o700))
	backend.signalLifecycle()
	assert.Equal(t, missing, requireReceiveWithin(t, released, time.Second),
		"native activation releases the transferred root-specific polling obligation")
}

func TestDarwinWatcherStartupCreateFailureSelectsRecursiveFallbackOnce(t *testing.T) {
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	second := filepath.Join(parent, "second")
	missing := filepath.Join(parent, "missing")
	require.NoError(t, os.Mkdir(first, 0o700))
	require.NoError(t, os.Mkdir(second, 0o700))
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	var creates atomic.Int32
	backend.newStream = func(string, func([]fsevents.Event)) (darwinStream, error) {
		creates.Add(1)
		return nil, errors.New("stream create unavailable")
	}
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil }, backend,
		defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes, WatcherOptions{},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)

	results := watcher.RegisterRoots([]WatchRoot{
		{Path: first, Recursive: true, Exists: true, Scopes: []WatchScope{{SyncDir: first}}},
		{Path: second, Recursive: true, Exists: true, Scopes: []WatchScope{{SyncDir: second}}},
		{Path: missing, Recursive: true, Scopes: []WatchScope{{SyncDir: missing}}},
	}, 1)

	assert.Equal(t, []RecursiveWatchResult{
		{Watched: 1},
		{Watched: 1},
		{Watched: 1},
	}, results)
	assert.Equal(t, int32(1), creates.Load(), "one failed stream selects global fallback")
	assert.Equal(t, []darwinFallbackPollPlan{
		{path: first, scopes: []PollingScope{{Root: first}}},
		{path: missing, scopes: []PollingScope{{Root: missing}}},
		{path: second, scopes: []PollingScope{{Root: second}}},
	}, backend.fallbackPollPlans)
	assert.Equal(t, uint32(1), backend.fallbackActivations.Load())
}

func TestDarwinWatcherStartupFallbackRetainsMissingShallowRootLifecycle(t *testing.T) {
	parent := t.TempDir()
	recursive := filepath.Join(parent, "recursive")
	missingShallow := filepath.Join(parent, "metadata")
	require.NoError(t, os.Mkdir(recursive, 0o700))
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	var creates atomic.Int32
	backend.newStream = func(string, func([]fsevents.Event)) (darwinStream, error) {
		if creates.Add(1) == 1 {
			return nil, errors.New("initial stream unavailable")
		}
		return &handoffRecordingStream{}, nil
	}
	polling := make(chan PollingObligation, 1)
	batches := make(chan WatchBatch, 8)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(_ context.Context, batch WatchBatch) error {
			batches <- batch
			return nil
		}, backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{OnPollingRequired: func(obligation PollingObligation) error {
			polling <- obligation
			return nil
		}},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	results := watcher.RegisterRoots([]WatchRoot{
		{
			Path: recursive, Recursive: true, Exists: true,
			Scopes: []WatchScope{{SyncDir: recursive}},
		},
		{
			Path:   missingShallow,
			Scopes: []WatchScope{{SyncDir: missingShallow}},
		},
	}, 10)
	assert.Equal(t, []RecursiveWatchResult{
		{Watched: 1},
		{Watched: 1, MissingRootLifecycleOwned: true},
	}, results)
	assert.Equal(t, 1, backend.ancestors[parent],
		"startup fallback must retain the missing shallow root's ancestor watch")
	require.NoError(t, watcher.Start())
	requireReceiveWithin(t, polling, time.Second)
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool { return batch.FullSync })

	require.NoError(t, os.Mkdir(missingShallow, 0o700))
	backend.signalLifecycle()
	require.Eventually(t, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		state := backend.logical[missingShallow]
		return state != nil && state.active && state.phase == darwinRootOpen &&
			backend.shallow[missingShallow] == 1
	}, time.Second, time.Millisecond)
	changed := filepath.Join(missingShallow, "session.jsonl")
	sent := make(chan struct{})
	go func() {
		backend.shallowWatcher.events <- backendEvent{Path: changed, Op: backendOpWrite}
		close(sent)
	}()
	batch := waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.Paths, changed)
	})
	requireReceiveWithin(t, sent, time.Second)
	assert.Contains(t, batch.Paths, changed)
}

func TestDarwinWatcherStartupStreamStartFailureFallsBack(t *testing.T) {
	root := t.TempDir()
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	var creates atomic.Int32
	backend.newStream = func(string, func([]fsevents.Event)) (darwinStream, error) {
		creates.Add(1)
		return &retryDarwinStream{startErr: errors.New("stream start unavailable")}, nil
	}
	batches := make(chan WatchBatch, 2)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(_ context.Context, batch WatchBatch) error {
			batches <- batch
			return nil
		}, backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{OnCoverageDegraded: func([]string) error { return nil }},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{SyncDir: root}},
	}}, 10)
	require.Equal(t, []RecursiveWatchResult{{Watched: 1}}, results)

	watcher.Start()
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool { return batch.FullSync })
	require.Eventually(t, func() bool {
		return backend.fallbackPhaseValue() == darwinFallbackOpen
	}, time.Second, time.Millisecond)
	assert.Eventually(t, func() bool { return creates.Load() >= 2 }, time.Second, time.Millisecond,
		"native stream start is retried while polling remains active")
	assert.Equal(t, uint32(1), backend.fallbackActivations.Load())
}

func TestDarwinWatcherStartupFallbackShallowFailurePollsEveryScope(t *testing.T) {
	root := t.TempDir()
	shallow := t.TempDir()
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	backend.newStream = func(string, func([]fsevents.Event)) (darwinStream, error) {
		return nil, errors.New("stream create unavailable")
	}
	backend.shallowWatcher.newStream = func(string, func([]fsevents.Event)) (darwinStream, error) {
		return &failingStartStream{err: errors.New("shallow FSEvents unavailable")}, nil
	}
	coverage := make(chan []string, 1)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil }, backend,
		defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{OnCoverageDegraded: func(roots []string) error {
			coverage <- append([]string(nil), roots...)
			return nil
		}},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{SyncDir: root}},
	}, {
		Path: shallow, Exists: true,
		Scopes: []WatchScope{{SyncDir: shallow}},
	}}, 10)
	require.Equal(t, []RecursiveWatchResult{{Watched: 1}, {Watched: 1}}, results)

	watcher.Start()
	assert.ElementsMatch(t, []string{root, shallow},
		requireReceiveWithin(t, coverage, time.Second))
	require.Eventually(t, func() bool {
		return backend.fallbackPhaseValue() == darwinFallbackOpen
	}, time.Second, time.Millisecond)
	_, retry := backend.nextLifecycleRetry()
	assert.False(t, retry,
		"an unrecoverable shallow start failure must not wake the lifecycle loop forever")
}

// failingStartStream is a stream whose Start always fails.
type failingStartStream struct{ err error }

func (s *failingStartStream) Start() error { return s.err }
func (s *failingStartStream) Close() error { return nil }

func TestDarwinWatcherRuntimeFallbackOrdersCoverageAndRetainsReconciliation(t *testing.T) {
	for _, reconcileErr := range []error{
		errors.New("reconciliation unavailable"),
		context.Canceled,
	} {
		t.Run(reconcileErr.Error(), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "sessions")
			require.NoError(t, os.Mkdir(root, 0o700))
			afterInvalidation := filepath.Join(root, "after-invalidation.jsonl")
			duringReconcile := filepath.Join(root, "during-reconcile.jsonl")
			backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
			require.NoError(t, err)
			backend.retryInitial = time.Second
			backend.retryMax = time.Second
			var nativeSink func([]fsevents.Event)
			order := make(chan string, 16)
			closed := make(chan struct{})
			invalidationDispatch := make(chan bool, 1)
			reconcileDispatch := make(chan bool, 1)
			backend.newStream = func(_ string, sink func([]fsevents.Event)) (darwinStream, error) {
				nativeSink = sink
				return &handoffRecordingStream{close: func() {
					order <- "invalidate"
					_, dispatch := backend.handleShallowEvent(backendEvent{
						Path: afterInvalidation, Op: backendOpWrite,
					})
					invalidationDispatch <- dispatch
					close(closed)
				}}, nil
			}
			coverageCalls := atomic.Int32{}
			batches := make(chan WatchBatch, 16)
			firstReconcile := make(chan struct{})
			retryStarted := make(chan WatchBatch, 1)
			releaseRetry := make(chan struct{})
			var releaseRetryOnce sync.Once
			allowRetry := func() { releaseRetryOnce.Do(func() { close(releaseRetry) }) }
			var attempts atomic.Int32
			watcher, err := newWatcherWithBackendOptions(
				0, time.Millisecond,
				func(_ context.Context, batch WatchBatch) error {
					batches <- batch
					if !batch.FullSync {
						return nil
					}
					order <- "reconcile"
					attempt := attempts.Add(1)
					if attempt == 1 {
						_, dispatch := backend.handleShallowEvent(backendEvent{
							Path: duringReconcile, Op: backendOpWrite,
						})
						reconcileDispatch <- dispatch
						close(firstReconcile)
						return reconcileErr
					}
					retryStarted <- batch
					<-releaseRetry
					return nil
				},
				backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
				WatcherOptions{OnCoverageDegraded: func(roots []string) error {
					coverageCalls.Add(1)
					order <- "poll"
					return nil
				}},
			)
			require.NoError(t, err)
			t.Cleanup(watcher.Stop)
			t.Cleanup(allowRetry)
			results := watcher.RegisterRoots([]WatchRoot{{
				Path: root, Recursive: true, Exists: true,
				Scopes: []WatchScope{{Agent: "agent-a", SyncDir: root}},
			}}, 10)
			require.Equal(t, []RecursiveWatchResult{{Watched: 1}}, results)
			watcher.Start()

			before := filepath.Join(root, "before-fallback.jsonl")
			nativeSink([]fsevents.Event{{
				Path: before, Flags: fseventFlagItemModified | fseventFlagItemIsFile,
			}})
			waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
				return slices.Contains(batch.Paths, before)
			})

			backend.requestFallback(darwinFallbackNativeDrop)
			requireReceiveWithin(t, firstReconcile, time.Second)
			requireReceiveWithin(t, closed, time.Second)
			assert.False(t, requireReceiveWithin(t, invalidationDispatch, time.Second))
			assert.False(t, requireReceiveWithin(t, reconcileDispatch, time.Second))
			// Batch delivery precedes the attempt counter update. Wait for the
			// retry callback itself so a fast receiver cannot discard its batch.
			retry := requireReceiveWithin(t, retryStarted, time.Second)
			assert.True(t, retry.FullSync)
			assert.Equal(t, darwinFallbackReconciling, backend.fallbackPhaseValue())
			assert.Equal(t, int32(2), attempts.Load())
			assert.Equal(t, int32(1), coverageCalls.Load())
			allowRetry()
			require.Eventually(t, func() bool {
				return backend.fallbackPhaseValue() == darwinFallbackOpen
			}, time.Second, time.Millisecond)

			retained := waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
				return slices.Contains(batch.Paths, duringReconcile)
			})
			assert.ElementsMatch(t, []string{
				afterInvalidation,
				duringReconcile,
			}, retained.Paths)
			assert.Equal(t, int32(2), attempts.Load(), "one retained marker retries until success")
			assert.Equal(t, uint32(1), backend.fallbackActivations.Load())

			gotOrder := make([]string, 0, 4)
			for len(gotOrder) < 4 {
				gotOrder = append(gotOrder, requireReceiveWithin(t, order, time.Second))
			}
			assert.Equal(t, []string{"poll", "invalidate", "reconcile", "reconcile"}, gotOrder)
		})
	}
}

func TestDarwinWatcherFallbackLifecycleTokenSurvivesSinkContention(t *testing.T) {
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	t.Cleanup(backend.Stop)
	sink := newWatchEventSink(
		defaultWatchBatchMaxEntries,
		defaultWatchBatchMaxPathBytes,
	)
	backend.bindEventSink(sink)

	sink.mu.Lock()
	backend.beginFallbackReconciliation()
	nativeAccepted := sink.TryAccumulate(func(add func(backendEvent) bool) bool {
		return add(backendEvent{Path: "/native.jsonl", Op: backendOpWrite})
	})
	sink.mu.Unlock()

	assert.True(t, nativeAccepted, "contended native delivery must remain nonblocking")
	batch, ok := sink.Take(nil)
	require.True(t, ok)
	assert.True(t, batch.FullSync)
	require.Len(t, batch.lifecycleTokens, 1)
	assert.Same(t, backend, batch.lifecycleTokens[0].gate)
	assert.Equal(t, uint64(1), batch.lifecycleTokens[0].generation)
	batch.lifecycleTokens[0].gate.acknowledgeLifecycle(
		batch.lifecycleTokens[0].generation,
	)
	assert.Equal(t, uint64(1), backend.fallbackAcknowledged.Load())
}

func TestDarwinWatcherRuntimeFallbackWaitsForPollingRegistration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	require.NoError(t, os.Mkdir(root, 0o700))
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	invalidated := make(chan struct{}, 1)
	backend.newStream = func(string, func([]fsevents.Event)) (darwinStream, error) {
		return &handoffRecordingStream{close: func() { invalidated <- struct{}{} }}, nil
	}
	allowPolling := make(chan struct{})
	unblockPolling := sync.OnceFunc(func() { close(allowPolling) })
	coverageCalls := make(chan []string, 2)
	var attempts atomic.Int32
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil }, backend,
		defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{OnCoverageDegraded: func(roots []string) error {
			coverageCalls <- append([]string(nil), roots...)
			if attempts.Add(1) == 1 {
				return errors.New("poll coordinator stopped")
			}
			<-allowPolling
			return nil
		}},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	// Release the parked polling callback before Stop waits for the backend.
	t.Cleanup(unblockPolling)
	watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{SyncDir: root}},
	}}, 10)
	backend.retryInitial = time.Millisecond
	backend.retryMax = 2 * time.Millisecond
	watcher.Start()

	backend.requestFallback(darwinFallbackNativeDrop)
	assert.Equal(t, []string{root}, requireReceiveWithin(t, coverageCalls, time.Second))
	assert.Equal(t, []string{root}, requireReceiveWithin(t, coverageCalls, time.Second))
	select {
	case <-invalidated:
		require.FailNow(t, "FSEvents invalidated before polling owned the uncovered root")
	default:
	}
	unblockPolling()
	requireReceiveWithin(t, invalidated, time.Second)
	assert.Equal(t, int32(2), attempts.Load())
}

// TestDarwinWatcherNativeDropReconcilesOnlyThatRoot is the work-count gate for
// issue 2567. A dropped FSEvents batch on one recursive root must cost one
// bounded reconciliation of that root's scopes. It must not register any
// unwatched-poll obligation, start the backend-wide fallback, or close any
// stream, and every root, including the one that dropped events, must keep
// delivering native events. A drop storm on the same root stays scoped to that
// root. The polling callbacks block forever, so a native callback that reached
// them would also fail the prompt-return check.
func TestDarwinWatcherNativeDropReconcilesOnlyThatRoot(t *testing.T) {
	base := t.TempDir()
	roots := []string{
		filepath.Join(base, "agent-a"),
		filepath.Join(base, "agent-b"),
		filepath.Join(base, "agent-c"),
	}
	for _, root := range roots {
		require.NoError(t, os.Mkdir(root, 0o700))
	}
	dropped := roots[1]

	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	var sinksMu sync.Mutex
	sinks := make(map[string]func([]fsevents.Event))
	var streamsCreated, streamsClosed atomic.Int32
	backend.newStream = func(root string, sink func([]fsevents.Event)) (darwinStream, error) {
		sinksMu.Lock()
		sinks[root] = sink
		sinksMu.Unlock()
		streamsCreated.Add(1)
		return &handoffRecordingStream{close: func() { streamsClosed.Add(1) }}, nil
	}
	sinkFor := func(root string) func([]fsevents.Event) {
		sinksMu.Lock()
		defer sinksMu.Unlock()
		sink := sinks[root]
		require.NotNil(t, sink, "no native stream for %s", root)
		return sink
	}

	var pollingCalls atomic.Int32
	releasePolling := make(chan struct{})
	unblockPolling := sync.OnceFunc(func() { close(releasePolling) })
	batches := make(chan WatchBatch, 64)
	// order records trust revocation and reconciliation delivery so the test
	// can require the first to precede the second.
	order := make(chan string, 256)
	var trustRevocations atomic.Int32
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(_ context.Context, batch WatchBatch) error {
			if len(batch.ReconcileRoots) > 0 {
				select {
				case order <- "reconcile":
				default:
				}
			}
			batches <- batch
			return nil
		}, backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{
			OnNativeEventsLost: func() {
				trustRevocations.Add(1)
				select {
				case order <- "trust":
				default:
				}
			},
			OnPollingRequired: func(PollingObligation) error {
				pollingCalls.Add(1)
				<-releasePolling
				return nil
			},
			OnPollingReleased: func(string) error { return nil },
			OnCoverageDegraded: func([]string) error {
				pollingCalls.Add(1)
				<-releasePolling
				return nil
			},
		},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	// Release any parked polling callback before Stop waits for the backend.
	t.Cleanup(unblockPolling)
	plan := make([]WatchRoot, len(roots))
	for i, root := range roots {
		plan[i] = WatchRoot{
			Path: root, Recursive: true, Exists: true,
			Scopes: []WatchScope{{Agent: filepath.Base(root), SyncDir: root}},
		}
	}
	require.Equal(t, []RecursiveWatchResult{{Watched: 1}, {Watched: 1}, {Watched: 1}},
		watcher.RegisterRoots(plan, 10))
	require.NoError(t, watcher.Start())
	require.Equal(t, int32(len(roots)), streamsCreated.Load())

	var seen []WatchBatch
	collect := func(match func(WatchBatch) bool) WatchBatch {
		t.Helper()
		batch := waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
			seen = append(seen, batch)
			return match(batch)
		})
		return batch
	}

	returned := make(chan struct{})
	go func() {
		sinkFor(dropped)([]fsevents.Event{{Flags: fseventFlagMustScanSubDirs}})
		close(returned)
	}()
	requireReceiveWithin(t, returned, 5*time.Second)
	reconcile := collect(func(batch WatchBatch) bool { return len(batch.ReconcileRoots) > 0 })
	assert.Equal(t, []string{dropped}, reconcile.ReconcileRoots,
		"a drop reconciles only the root whose stream lost events")
	assert.False(t, reconcile.FullSync, "a single-root drop must not become a full rescan")
	assert.False(t, reconcile.LostEvents,
		"a single-root drop must not clear archive-wide freshness caches")
	assert.Equal(t, "trust", requireReceiveWithin(t, order, 5*time.Second),
		"event-dependent trust must be revoked before the root is reconciled")

	// A storm of drops on the same root.
	for range 100 {
		sinkFor(dropped)([]fsevents.Event{{Flags: fseventFlagMustScanSubDirs}})
	}

	// Native watching continues on every root, including the one that dropped.
	for _, root := range roots {
		path := filepath.Join(root, "after-drop.jsonl")
		sinkFor(root)([]fsevents.Event{{
			Path: path, Flags: fseventFlagItemModified | fseventFlagItemIsFile,
		}})
		collect(func(batch WatchBatch) bool { return slices.Contains(batch.Paths, path) })
	}

	for _, batch := range seen {
		assert.False(t, batch.FullSync, "no batch may escalate to a full rescan")
		for _, root := range batch.ReconcileRoots {
			assert.Equal(t, dropped, root, "only the dropping root may be reconciled")
		}
	}
	assert.Zero(t, pollingCalls.Load(), "a native drop must not register unwatched polling")
	assert.Zero(t, streamsClosed.Load(), "a native drop must not close any stream")
	assert.Equal(t, int32(len(roots)), streamsCreated.Load(),
		"a native drop must not recreate streams")
	assert.Equal(t, darwinFallbackInactive, backend.fallbackPhaseValue())
	assert.Equal(t, darwinFallbackReasonNone,
		darwinFallbackReason(backend.fallbackReason.Load()))
	assert.Zero(t, backend.fallbackActivations.Load())
	assert.Equal(t, uint32(101), backend.nativeDrops.Load())
	assert.Positive(t, trustRevocations.Load())
}

// TestDarwinWatcherNativeDropRecoversHiddenRootLoss covers a drop that hides
// the root's own removal or replacement. The FSEvents bridge collapses a batch
// with a drop flag into one pathless must-rescan event, so a RootChanged flag
// in that batch is lost. The lifecycle worker must notice that the root no
// longer names the directory its stream opened on and run root-local loss
// recovery: close only that stream and hand only that root to polling. A
// replaced root then gets a new stream and releases its polling.
func TestDarwinWatcherNativeDropRecoversHiddenRootLoss(t *testing.T) {
	replace := func(t *testing.T, root string) {
		require.NoError(t, os.Rename(root, root+".old"))
		require.NoError(t, os.Mkdir(root, 0o700))
	}
	for _, tc := range []struct {
		name     string
		mutate   func(t *testing.T, root string)
		recovers bool
		// duringActivation replaces the root while its first stream is being
		// created, so the identity recorded at activation must be the
		// directory that existed before the stream, not its replacement.
		duringActivation bool
	}{
		{name: "removed", mutate: func(t *testing.T, root string) {
			require.NoError(t, os.RemoveAll(root))
		}},
		{name: "replaced", recovers: true, mutate: replace},
		{name: "replaced during activation", recovers: true, mutate: replace,
			duringActivation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			healthy := filepath.Join(base, "agent-a")
			dropped := filepath.Join(base, "agent-b")
			for _, root := range []string{healthy, dropped} {
				require.NoError(t, os.Mkdir(root, 0o700))
			}
			backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
			require.NoError(t, err)
			backend.retryInitial = 5 * time.Millisecond
			backend.retryMax = 10 * time.Millisecond
			var sinksMu sync.Mutex
			sinks := make(map[string]func([]fsevents.Event))
			created := make(map[string]int)
			closed := make(map[string]int)
			backend.newStream = func(root string, sink func([]fsevents.Event)) (darwinStream, error) {
				sinksMu.Lock()
				defer sinksMu.Unlock()
				if tc.duringActivation && root == dropped && created[root] == 0 {
					tc.mutate(t, root)
				}
				sinks[root] = sink
				created[root]++
				return &handoffRecordingStream{close: func() {
					sinksMu.Lock()
					closed[root]++
					sinksMu.Unlock()
				}}, nil
			}
			counts := func(root string) (int, int) {
				sinksMu.Lock()
				defer sinksMu.Unlock()
				return created[root], closed[root]
			}
			polling := make(chan PollingObligation, 8)
			released := make(chan string, 8)
			watcher, err := newWatcherWithBackendOptions(
				0, 0, func(context.Context, WatchBatch) error { return nil },
				backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
				WatcherOptions{
					OnPollingRequired: func(obligation PollingObligation) error {
						polling <- obligation
						return nil
					},
					OnPollingReleased: func(key string) error {
						released <- key
						return nil
					},
				},
			)
			require.NoError(t, err)
			t.Cleanup(watcher.Stop)
			watcher.RegisterRoots([]WatchRoot{
				{
					Path: healthy, Recursive: true, Exists: true,
					Scopes: []WatchScope{{Agent: "agent-a", SyncDir: healthy}},
				},
				{
					Path: dropped, Recursive: true, Exists: true,
					Scopes: []WatchScope{{Agent: "agent-b", SyncDir: dropped}},
				},
			}, 10)
			require.NoError(t, watcher.Start())

			if !tc.duringActivation {
				tc.mutate(t, dropped)
			}
			sinksMu.Lock()
			sink := sinks[dropped]
			sinksMu.Unlock()
			sink([]fsevents.Event{{Flags: fseventFlagMustScanSubDirs}})

			obligation := requireReceiveWithin(t, polling, 5*time.Second)
			assert.Equal(t, dropped, obligation.Key,
				"only the root whose loss the drop hid may move to polling")
			require.Eventually(t, func() bool {
				_, closedDropped := counts(dropped)
				return closedDropped == 1
			}, 5*time.Second, time.Millisecond)
			if tc.recovers {
				assert.Equal(t, dropped, requireReceiveWithin(t, released, 5*time.Second))
				createdDropped, _ := counts(dropped)
				assert.Equal(t, 2, createdDropped,
					"the replaced root must get a stream on its new directory")
			}
			createdHealthy, closedHealthy := counts(healthy)
			assert.Equal(t, 1, createdHealthy)
			assert.Zero(t, closedHealthy, "other roots keep their streams")
			assert.Equal(t, darwinFallbackInactive, backend.fallbackPhaseValue())
			assert.Zero(t, backend.fallbackActivations.Load())
		})
	}
}

func TestDarwinWatcherShallowStartFailureFallbackPollsBeforeInvalidation(t *testing.T) {
	root := t.TempDir()
	shallow := filepath.Join(root, "shallow")
	require.NoError(t, os.Mkdir(shallow, 0o700))
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	backend.startShallow = func() error { return errors.New("shallow FSEvents unavailable") }
	var nativeSink func([]fsevents.Event)
	pollingOwned := atomic.Bool{}
	invalidated := make(chan struct{})
	invalidatedWithPolling := make(chan bool, 1)
	coverage := make(chan []string, 1)
	backend.newStream = func(_ string, sink func([]fsevents.Event)) (darwinStream, error) {
		nativeSink = sink
		return &handoffRecordingStream{close: func() {
			invalidatedWithPolling <- pollingOwned.Load()
			close(invalidated)
		}}, nil
	}
	batches := make(chan WatchBatch, 2)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(_ context.Context, batch WatchBatch) error {
			batches <- batch
			return nil
		}, backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{OnCoverageDegraded: func(roots []string) error {
			coverage <- append([]string(nil), roots...)
			pollingOwned.Store(true)
			return nil
		}},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	watcher.RegisterRoots([]WatchRoot{
		{
			Path: root, Recursive: true, Exists: true,
			Scopes: []WatchScope{{SyncDir: root}},
		},
		{
			Path: shallow, Exists: true,
			Scopes: []WatchScope{{SyncDir: shallow}},
		},
	}, 10)
	watcher.Start()

	require.NotNil(t, nativeSink)
	assert.Equal(t, []string{root, shallow}, requireReceiveWithin(t, coverage, time.Second))
	requireReceiveWithin(t, invalidated, time.Second)
	assert.True(t, requireReceiveWithin(t, invalidatedWithPolling, time.Second))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool { return batch.FullSync })
	require.Eventually(t, func() bool {
		return backend.fallbackPhaseValue() == darwinFallbackOpen
	}, time.Second, time.Millisecond)
}

func TestDarwinWatcherLifecycleFailureFallsBackToPolling(t *testing.T) {
	ancestor := t.TempDir()
	intermediate := filepath.Join(ancestor, "state")
	root := filepath.Join(intermediate, "sessions")
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	backend.retryInitial = time.Millisecond
	backend.retryMax = 5 * time.Millisecond
	var attempts atomic.Int32
	backend.addAncestor = func(path string) error {
		if path == intermediate && attempts.Add(1) == 1 {
			return errors.New("descriptor unavailable")
		}
		return nil
	}
	backend.removeAncestor = func(string) error { return nil }
	coverage := make(chan []string, 1)
	batches := make(chan WatchBatch, 8)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(_ context.Context, batch WatchBatch) error {
			batches <- batch
			return nil
		}, backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{OnCoverageDegraded: func(roots []string) error {
			coverage <- append([]string(nil), roots...)
			return nil
		}},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	require.Equal(t, RecursiveWatchResult{
		Watched: 1, MissingRootLifecycleOwned: true,
	}, results[0])
	watcher.Start()
	require.NoError(t, os.Mkdir(intermediate, 0o700))
	backend.signalLifecycle()

	assert.Equal(t, []string{ancestor}, requireReceiveWithin(t, coverage, time.Second))
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return batch.FullSync
	})
	require.Eventually(t, func() bool {
		return backend.fallbackPhaseValue() == darwinFallbackOpen
	}, time.Second, time.Millisecond)
	assert.Equal(t, int32(1), attempts.Load())
}

func TestDarwinWatcherShallowLifecycleFailureFallbackPollsScope(t *testing.T) {
	ancestor := t.TempDir()
	intermediate := filepath.Join(ancestor, "state")
	root := filepath.Join(intermediate, "sessions")
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	backend.retryInitial = time.Millisecond
	backend.retryMax = 5 * time.Millisecond
	backend.addAncestor = func(path string) error {
		if path == intermediate {
			return errors.New("descriptor unavailable")
		}
		return nil
	}
	backend.removeAncestor = func(string) error { return nil }
	coverage := make(chan []string, 1)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil }, backend,
		defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{OnCoverageDegraded: func(roots []string) error {
			coverage <- append([]string(nil), roots...)
			return nil
		}},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path:   root,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	require.Equal(t, RecursiveWatchResult{
		Watched: 1, MissingRootLifecycleOwned: true,
	}, results[0])
	watcher.Start()
	require.NoError(t, os.Mkdir(intermediate, 0o700))
	backend.signalLifecycle()

	assert.Equal(t, []string{ancestor}, requireReceiveWithin(t, coverage, time.Second))
}

func TestDarwinWatcherMissingRecursiveSymlinkStaysPolled(t *testing.T) {
	ancestor := t.TempDir()
	root := filepath.Join(ancestor, "sessions")
	target := t.TempDir()
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	backend.retryInitial = time.Millisecond
	backend.retryMax = 5 * time.Millisecond
	var streamCalls atomic.Int32
	backend.newStream = func(string, func([]fsevents.Event)) (darwinStream, error) {
		streamCalls.Add(1)
		return &handoffRecordingStream{}, nil
	}
	required := make(chan PollingObligation, 1)
	batches := make(chan WatchBatch, 8)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(_ context.Context, batch WatchBatch) error {
			batches <- batch
			return nil
		}, backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{OnPollingRequired: func(obligation PollingObligation) error {
			required <- obligation
			return nil
		}},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	require.Equal(t, RecursiveWatchResult{
		Watched: 1, MissingRootLifecycleOwned: true,
	}, results[0])
	var firstRootInspection atomic.Bool
	symlinkResult := make(chan error, 1)
	backend.lstat = func(path string) (os.FileInfo, error) {
		if path == root && firstRootInspection.CompareAndSwap(false, true) {
			symlinkResult <- os.Symlink(target, root)
			return nil, os.ErrNotExist
		}
		return os.Lstat(path)
	}
	watcher.Start()
	backend.signalLifecycle()
	require.NoError(t, requireReceiveWithin(t, symlinkResult, time.Second))

	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.ReconcileRoots, ancestor)
	})
	assert.Equal(t, PollingObligation{Key: root, Scopes: []PollingScope{{Agent: "agent-a", Root: ancestor}}, Probe: root},
		requireReceiveWithin(t, required, time.Second))
	require.True(t, firstRootInspection.Load())
	backend.mu.Lock()
	phase := backend.logical[root].phase
	backend.mu.Unlock()
	assert.Equal(t, darwinRootPolling, phase)
	assert.Zero(t, streamCalls.Load(),
		"a recursive symlink is owned by configured polling, not a native stream")
}

func TestDarwinWatcherMissingShallowSymlinkActivatesAndRestoresCoverage(t *testing.T) {
	ancestor := t.TempDir()
	root := filepath.Join(ancestor, "sessions")
	target := t.TempDir()
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	backend.retryInitial = time.Millisecond
	backend.retryMax = 5 * time.Millisecond
	restored := make(chan []string, 1)
	batches := make(chan WatchBatch, 4)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(_ context.Context, batch WatchBatch) error {
			batches <- batch
			return nil
		}, backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		WatcherOptions{OnPollingReleased: func(key string) error {
			restored <- []string{key}
			return nil
		}},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path:   root,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
	}}, 10)
	require.Equal(t, RecursiveWatchResult{
		Watched: 1, MissingRootLifecycleOwned: true,
	}, results[0])
	require.NoError(t, watcher.Start())
	require.NoError(t, os.Symlink(target, root))
	backend.signalLifecycle()

	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		return slices.Contains(batch.ReconcileRoots, ancestor)
	})
	assert.Equal(t, []string{root}, requireReceiveWithin(t, restored, time.Second))
	require.Eventually(t, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		state := backend.logical[root]
		return state != nil && state.active && state.phase == darwinRootOpen
	}, time.Second, time.Millisecond)
}

func TestDarwinWatcherPendingLossWinsOverActivationAcknowledgement(t *testing.T) {
	ancestor := t.TempDir()
	root := filepath.Join(ancestor, "sessions")
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	backend.lifecycle = darwinBackendRunning
	backend.addAncestor = func(string) error { return nil }
	backend.removeAncestor = func(string) error { return nil }
	required := make(chan PollingObligation, 1)
	backend.onPollingRequired = func(obligation PollingObligation) error {
		required <- obligation
		return nil
	}
	closed := make(chan struct{}, 1)
	stream := &handoffRecordingStream{close: func() { closed <- struct{}{} }}
	state := &darwinLogicalRoot{
		plan: WatchRoot{
			Path: root, Recursive: true,
			Scopes: []WatchScope{{Agent: "agent-a", SyncDir: ancestor}},
		},
		active:     true,
		stream:     stream,
		phase:      darwinRootActivationCollecting,
		generation: 1,
		retryAt:    time.Now().Add(time.Hour),
	}
	state.currentGeneration.Store(1)
	state.acknowledged.Store(1)
	state.signal.Or(darwinRootSignalLoss)
	backend.logical[root] = state
	backend.streams[root] = stream

	backend.processLifecycleSignals()

	assert.Equal(t, darwinRootLossCollecting, state.phase)
	assert.False(t, state.active)
	assert.Equal(t, PollingObligation{Key: root, Scopes: []PollingScope{{Agent: "agent-a", Root: ancestor}}, Probe: root},
		requireReceiveWithin(t, required, time.Second))
	requireReceiveWithin(t, closed, time.Second)
}

type handoffRecordingStream struct {
	start func()
	close func()
}

func (s *handoffRecordingStream) Start() error {
	if s.start != nil {
		s.start()
	}
	return nil
}

func (s *handoffRecordingStream) Close() error {
	if s.close != nil {
		s.close()
	}
	return nil
}

type darwinDirectTestBackend struct {
	events chan backendEvent
	errors chan error
	sink   directBackendEventSink
}

type stopAfterOneDirectSink struct {
	added      int
	event      backendEvent
	meaningful bool
}

func (s *stopAfterOneDirectSink) TryAccumulate(
	accumulate func(func(backendEvent) bool) bool,
) bool {
	s.meaningful = accumulate(func(event backendEvent) bool {
		s.added++
		s.event = event
		return false
	})
	return true
}

func (s *stopAfterOneDirectSink) RetainAuthoritative(token backendLifecycleToken) {
	s.added++
	s.event = backendEvent{Op: backendOpFullSync, Lifecycle: token}
	s.meaningful = true
}

func newDarwinDirectTestBackend() *darwinDirectTestBackend {
	return &darwinDirectTestBackend{
		events: make(chan backendEvent),
		errors: make(chan error),
	}
}

func (b *darwinDirectTestBackend) Events() <-chan backendEvent { return b.events }
func (b *darwinDirectTestBackend) Errors() <-chan error        { return b.errors }
func (b *darwinDirectTestBackend) AddRecursive(string, int) RecursiveWatchResult {
	return RecursiveWatchResult{}
}
func (b *darwinDirectTestBackend) AddShallow(string) error { return nil }
func (b *darwinDirectTestBackend) Remove(string) error     { return nil }
func (b *darwinDirectTestBackend) Start() error            { return nil }
func (b *darwinDirectTestBackend) Stop()                   {}
func (b *darwinDirectTestBackend) Name() string            { return "darwin-direct-test" }
func (b *darwinDirectTestBackend) bindEventSink(sink directBackendEventSink) {
	b.sink = sink
}

func (b *darwinDirectTestBackend) emit(events []backendEvent) bool {
	return b.sink.TryAccumulate(func(add func(backendEvent) bool) bool {
		for _, event := range events {
			if !add(event) {
				return true
			}
		}
		return len(events) > 0
	})
}

func (b *darwinDirectTestBackend) emitFiltered() bool {
	return b.sink.TryAccumulate(func(func(backendEvent) bool) bool {
		return false
	})
}

func (b *darwinDirectTestBackend) sendShallow(t *testing.T, event backendEvent) {
	t.Helper()
	sent := make(chan struct{})
	go func() {
		b.events <- event
		close(sent)
	}()
	requireReceiveWithin(t, sent, time.Second)
}

func newDarwinTestWatcher(
	t *testing.T,
	root string,
	excludes []string,
	latency time.Duration,
) (*Watcher, <-chan WatchBatch) {
	t.Helper()

	backend, err := newDarwinWatchBackend(excludes, latency)
	require.NoError(t, err)
	watcher, batches := newDarwinTestWatcherWithBackend(t, backend)
	result := watcher.WatchRecursiveBudgeted(root, 1)
	require.NoError(t, result.Err)
	require.Equal(t, 1, result.Watched)
	return watcher, batches
}

func newDarwinTestWatcherWithBackend(
	t *testing.T,
	backend *darwinWatchBackend,
) (*Watcher, <-chan WatchBatch) {
	t.Helper()
	batches := make(chan WatchBatch, 32)
	watcher, err := newWatcherWithBackend(
		500*time.Millisecond,
		20*time.Millisecond,
		func(_ context.Context, batch WatchBatch) error {
			batches <- batch
			return nil
		},
		backend,
		defaultWatchBatchMaxEntries,
		defaultWatchBatchMaxPathBytes,
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	return watcher, batches
}

func waitForDarwinBatch(
	t *testing.T,
	batches <-chan WatchBatch,
	match func(WatchBatch) bool,
) WatchBatch {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case batch := <-batches:
			if match(batch) {
				return batch
			}
		case <-timer.C:
			stacktest.Dump(t)
			require.FailNow(t, "timed out waiting for matching Darwin watcher batch")
			return WatchBatch{}
		}
	}
}
