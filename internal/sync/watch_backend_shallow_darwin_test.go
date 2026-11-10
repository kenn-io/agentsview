//go:build darwin && cgo

package sync

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/fsevents"
)

func TestTranslateShallowFSEventKeepsRootAndDirectChildren(t *testing.T) {
	const root = "/sessions"
	for _, tc := range []struct {
		name  string
		path  string
		flags fsevents.EventFlags
		want  backendEvent
		keep  bool
	}{
		{
			name:  "direct child file write",
			path:  "/sessions/a.jsonl",
			flags: fseventFlagItemModified | fseventFlagItemIsFile,
			want: backendEvent{
				Path: "/sessions/a.jsonl", Root: root,
				Op: backendOpWrite, ItemType: backendItemFile,
			},
			keep: true,
		},
		{
			name:  "direct child directory create",
			path:  "/sessions/conversation",
			flags: fseventFlagItemCreated | fseventFlagItemIsDir,
			want: backendEvent{
				Path: "/sessions/conversation", Root: root,
				Op: backendOpCreate, ItemType: backendItemDirectory,
			},
			keep: true,
		},
		{
			name:  "grandchild write",
			path:  "/sessions/conversation/events.jsonl",
			flags: fseventFlagItemModified | fseventFlagItemIsFile,
		},
		{
			name:  "root contents changed",
			path:  "/sessions",
			flags: fseventFlagItemModified | fseventFlagItemIsDir,
		},
		{
			name:  "root renamed",
			path:  "/sessions",
			flags: fseventFlagItemRenamed | fseventFlagItemIsDir,
			want: backendEvent{
				Path: root, Root: root,
				Op: backendOpRename, ItemType: backendItemDirectory,
			},
			keep: true,
		},
		{
			name:  "root replaced",
			path:  "/",
			flags: fseventFlagRootChanged,
			want: backendEvent{
				Path: root, Root: root,
				Op: backendOpRemove, ItemType: backendItemDirectory,
			},
			keep: true,
		},
		{
			name:  "sibling outside root",
			path:  "/sessions-old/a.jsonl",
			flags: fseventFlagItemModified | fseventFlagItemIsFile,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, keep := translateShallowFSEvent(root, tc.path, tc.flags)
			assert.Equal(t, tc.keep, keep)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDarwinShallowWatcherCollapsesOverflowToOneFullSync(t *testing.T) {
	watcher := newDarwinShallowWatcher(nil, nil)
	t.Cleanup(watcher.Stop)

	// The native adapter caps one callback at defaultWatchBatchMaxEntries
	// events, so overflow comes from several callbacks queued before the
	// forwarder drains them. Each batch alone fits; together they do not.
	root := darwinShallowRoot{logicalPath: "/sessions", nativePath: "/sessions"}
	batch := make([]fsevents.Event, 5000)
	for i := range batch {
		batch[i] = fsevents.Event{
			Path:  "/sessions/a.jsonl",
			Flags: fseventFlagItemModified | fseventFlagItemIsFile,
		}
	}
	watcher.consume(root, batch)
	watcher.consume(root, batch)
	require.NoError(t, watcher.Start())

	assert.Equal(t, backendEvent{Op: backendOpFullSync},
		requireReceiveWithin(t, watcher.Events(), time.Second))
	watcher.consume(root, []fsevents.Event{{
		Path: "/sessions/after.jsonl", Flags: fseventFlagItemModified | fseventFlagItemIsFile,
	}})
	assert.Equal(t, "/sessions/after.jsonl",
		requireReceiveWithin(t, watcher.Events(), time.Second).Path,
		"delivery must resume with ordinary events after the full sync")
}

// drainingStream fails to start, and its Close waits for an in-flight
// callback the way fsevents.Stream.Close drains the shared dispatch queue.
type drainingStream struct {
	startErr error
	inFlight <-chan struct{}
	closed   chan struct{}
}

func (s *drainingStream) Start() error { return s.startErr }

func (s *drainingStream) Close() error {
	<-s.inFlight
	close(s.closed)
	return nil
}

func TestDarwinShallowWatcherFailedStartDoesNotDeadlockCallbacks(t *testing.T) {
	other := darwinShallowRoot{logicalPath: "/other", nativePath: "/other"}
	inFlight := make(chan struct{})
	startErr := errors.New("start refused")
	var watcher *darwinShallowWatcher
	stream := &drainingStream{
		startErr: startErr, inFlight: inFlight, closed: make(chan struct{}),
	}
	watcher = newDarwinShallowWatcher(nil,
		func(string, func([]fsevents.Event)) (darwinStream, error) {
			// Another stream's callback arrives while AddShallow is starting
			// this one, so it waits for the watcher's lock.
			go func() {
				defer close(inFlight)
				watcher.consume(other, []fsevents.Event{{
					Path:  "/other/a.jsonl",
					Flags: fseventFlagItemModified | fseventFlagItemIsFile,
				}})
			}()
			return stream, nil
		})
	require.NoError(t, watcher.Start())
	t.Cleanup(watcher.Stop)

	added := make(chan error, 1)
	go func() { added <- watcher.AddShallow("/sessions") }()

	require.ErrorIs(t, requireReceiveWithin(t, added, 10*time.Second), startErr)
	requireReceiveWithin(t, stream.closed, time.Second)
	assert.Zero(t, watcher.streamCount())
	assert.Equal(t, "/other/a.jsonl",
		requireReceiveWithin(t, watcher.Events(), time.Second).Path,
		"the waiting callback's event must still be delivered")
}

func TestDarwinWatcherShallowRootIgnoresNestedWrites(t *testing.T) {
	root := t.TempDir()
	conversation := filepath.Join(root, "conversation")
	require.NoError(t, os.Mkdir(conversation, 0o700))
	backend, err := newDarwinWatchBackend(nil, 20*time.Millisecond)
	require.NoError(t, err)
	watcher, batches := newDarwinTestWatcherWithBackend(t, backend)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Exists: true,
		Scopes: []WatchScope{{Agent: "agent-a", SyncDir: root}},
	}}, 10)
	require.Equal(t, RecursiveWatchResult{Watched: 1}, results[0])
	watcher.Start()

	nested := filepath.Join(conversation, "events.jsonl")
	require.NoError(t, os.WriteFile(nested, []byte("nested"), 0o600))
	child := filepath.Join(root, "index.json")
	require.NoError(t, os.WriteFile(child, []byte("child"), 0o600))

	var seen []string
	waitForDarwinBatch(t, batches, func(batch WatchBatch) bool {
		seen = append(seen, batch.Paths...)
		return slices.Contains(batch.Paths, child)
	})
	assert.NotContains(t, seen, nested,
		"a shallow root must not report writes below its direct children")
}
