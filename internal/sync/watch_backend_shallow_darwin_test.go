//go:build darwin && cgo

package sync

import (
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
	require.NoError(t, watcher.Start())
	t.Cleanup(watcher.Stop)

	root := darwinShallowRoot{logicalPath: "/sessions", nativePath: "/sessions"}
	events := make([]fsevents.Event, defaultWatchBatchMaxEntries+1)
	for i := range events {
		events[i] = fsevents.Event{
			Path:  "/sessions/a.jsonl",
			Flags: fseventFlagItemModified | fseventFlagItemIsFile,
		}
	}
	watcher.consume(root, events)

	assert.Equal(t, backendEvent{Op: backendOpFullSync},
		requireReceiveWithin(t, watcher.Events(), time.Second))
	watcher.consume(root, []fsevents.Event{{
		Path: "/sessions/after.jsonl", Flags: fseventFlagItemModified | fseventFlagItemIsFile,
	}})
	assert.Equal(t, "/sessions/after.jsonl",
		requireReceiveWithin(t, watcher.Events(), time.Second).Path,
		"delivery must resume with ordinary events after the full sync")
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
