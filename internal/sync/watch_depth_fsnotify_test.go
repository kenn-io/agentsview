//go:build !darwin

package sync

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

// depthLimitedTree builds root/{a,b} plus root/a/deep/deeper and returns root.
func depthLimitedTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "deep", "deeper"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "b"), 0o755))
	return root
}

func TestFSNotifyBackendAddRecursiveStopsAtMaxDepth(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := depthLimitedTree(t)
	backend.setWatchRootPlan([]WatchRoot{{
		Path: root, Recursive: true, MaxDepth: 1, Exists: true,
	}})

	// A budget that covers the depth-limited tree exactly must not be
	// reported as exhausted by the unwatched deeper directories.
	result := backend.AddRecursive(root, 3)

	require.NoError(t, result.Err)
	assert.False(t, result.BudgetExhausted)
	assert.Equal(t, 3, result.Watched)
	assert.ElementsMatch(t, []string{
		root, filepath.Join(root, "a"), filepath.Join(root, "b"),
	}, backend.watcher.WatchList())
}

func TestFSNotifyBackendRuntimeCreateHonorsMaxDepth(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := depthLimitedTree(t)
	backend.setWatchRootPlan([]WatchRoot{{
		Path: root, Recursive: true, MaxDepth: 1, Exists: true,
	}})
	require.NoError(t, backend.AddRecursive(root, math.MaxInt).Err)

	created := filepath.Join(root, "c")
	require.NoError(t, os.MkdirAll(filepath.Join(created, "inner"), 0o755))
	itemType, excluded := backend.watchCreatedPath(created)
	assert.Equal(t, backendItemDirectory, itemType)
	assert.False(t, excluded)

	tooDeep := filepath.Join(root, "b", "new")
	require.NoError(t, os.Mkdir(tooDeep, 0o755))
	_, excluded = backend.watchCreatedPath(tooDeep)
	assert.True(t, excluded, "a directory past the depth limit is not watched")

	watched := backend.watcher.WatchList()
	assert.Contains(t, watched, created)
	assert.NotContains(t, watched, filepath.Join(created, "inner"))
	assert.NotContains(t, watched, tooDeep)
}

func TestFSNotifyBackendCreatedSubtreeFilterHonorsMaxDepth(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := t.TempDir()
	backend.setWatchRootPlan([]WatchRoot{{
		Path: root, Recursive: true, MaxDepth: 1, Exists: true,
		ExtraDirectories: []string{"*/.system_generated/logs"},
	}})
	id := filepath.Join(root, "id")

	assert.True(t, backend.includeCreatedSubtreePath(root, id, true))
	assert.True(t, backend.includeCreatedSubtreePath(
		root, filepath.Join(id, "plan.md"), false,
	), "files inside the deepest watched directory stay covered")
	assert.False(t, backend.includeCreatedSubtreePath(
		root, filepath.Join(id, "generated"), true,
	), "directories below the depth limit are not walked")
	assert.False(t, backend.includeCreatedSubtreePath(
		root, filepath.Join(id, "generated", "note.md"), false,
	))

	generated := filepath.Join(id, ".system_generated")
	logs := filepath.Join(generated, "logs")
	assert.True(t, backend.includeCreatedSubtreePath(root, generated, true),
		"directories on the way to an extra directory are walked")
	assert.False(t, backend.includeCreatedSubtreePath(
		root, filepath.Join(generated, "noise.txt"), false,
	), "files in an intermediate directory stay ignored")
	assert.False(t, backend.includeCreatedSubtreePath(
		root, filepath.Join(generated, "other"), true,
	))
	assert.True(t, backend.includeCreatedSubtreePath(root, logs, true))
	assert.True(t, backend.includeCreatedSubtreePath(
		root, filepath.Join(logs, "transcript.jsonl"), false,
	))

	unlimited := t.TempDir()
	assert.True(t, backend.includeCreatedSubtreePath(
		unlimited, filepath.Join(unlimited, "a", "b", "c"), true,
	), "roots without a depth limit are unaffected")
	assert.True(t, backend.includeCreatedSubtreePath(
		unlimited, filepath.Join(unlimited, "a", "b", "c", "note.md"), false,
	))
}

func TestFSNotifyBackendTranslateEventKeepsFilesAtMaxDepth(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := t.TempDir()
	id := filepath.Join(root, "id")
	require.NoError(t, os.MkdirAll(id, 0o755))
	backend.setWatchRootPlan([]WatchRoot{{
		Path: root, Recursive: true, MaxDepth: 1, Exists: true,
	}})
	require.NoError(t, backend.AddRecursive(root, math.MaxInt).Err)

	artifact := filepath.Join(id, "artifact.md")
	for _, op := range []fsnotify.Op{
		fsnotify.Create,
		fsnotify.Write,
		fsnotify.Remove,
		fsnotify.Rename,
	} {
		event, relevant := backend.translateEvent(fsnotify.Event{Name: artifact, Op: op})
		assert.Truef(t, relevant, "op %v", op)
		assert.Equal(t, filepath.Clean(artifact), event.Path)
	}

	generated := filepath.Join(id, "generated")
	require.NoError(t, os.Mkdir(generated, 0o755))
	_, relevant := backend.translateEvent(fsnotify.Event{
		Name: generated, Op: fsnotify.Create,
	})
	assert.False(t, relevant, "a directory past the depth limit is not watched")
	assert.NotContains(t, backend.watcher.WatchList(), generated)

	_, relevant = backend.translateEvent(fsnotify.Event{
		Name: filepath.Join(generated, "note.md"), Op: fsnotify.Write,
	})
	assert.False(t, relevant, "a file below the deepest watched directory is ignored")
}

func TestFSNotifyBackendExtraDirectoriesWatchOnlyTheListedPath(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := t.TempDir()
	id := filepath.Join(root, "id")
	logs := filepath.Join(id, ".system_generated", "logs")
	sibling := filepath.Join(id, ".system_generated", "other")
	gitDir := filepath.Join(id, ".git", "objects")
	require.NoError(t, os.MkdirAll(logs, 0o755))
	require.NoError(t, os.MkdirAll(sibling, 0o755))
	require.NoError(t, os.MkdirAll(gitDir, 0o755))
	backend.setWatchRootPlan([]WatchRoot{{
		Path: root, Recursive: true, MaxDepth: 1, Exists: true,
		ExtraDirectories: []string{"*/.system_generated/logs"},
	}})

	result := backend.AddRecursive(root, math.MaxInt)
	require.NoError(t, result.Err)
	assert.False(t, result.BudgetExhausted)

	watched := backend.watcher.WatchList()
	assert.Contains(t, watched, root)
	assert.Contains(t, watched, id)
	assert.Contains(t, watched, filepath.Join(id, ".system_generated"))
	assert.Contains(t, watched, logs)
	assert.NotContains(t, watched, sibling)
	assert.NotContains(t, watched, filepath.Join(id, ".git"))
	assert.NotContains(t, watched, gitDir)

	transcript := filepath.Join(logs, "transcript.jsonl")
	event, relevant := backend.translateEvent(fsnotify.Event{
		Name: transcript, Op: fsnotify.Write,
	})
	assert.True(t, relevant)
	assert.Equal(t, filepath.Clean(transcript), event.Path)

	_, relevant = backend.translateEvent(fsnotify.Event{
		Name: filepath.Join(id, ".system_generated", "noise.txt"), Op: fsnotify.Write,
	})
	assert.False(t, relevant, "files in an intermediate directory stay ignored")

	_, relevant = backend.translateEvent(fsnotify.Event{
		Name: filepath.Join(id, "plan.md"), Op: fsnotify.Write,
	})
	assert.True(t, relevant, "files inside the depth limit stay visible")
}

func TestFSNotifyBackendRuntimeCreateFollowsExtraDirectories(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := t.TempDir()
	backend.setWatchRootPlan([]WatchRoot{{
		Path: root, Recursive: true, MaxDepth: 1, Exists: true,
		ExtraDirectories: []string{"*/.system_generated/logs"},
	}})
	require.NoError(t, backend.AddRecursive(root, math.MaxInt).Err)

	id := filepath.Join(root, "new-id")
	require.NoError(t, os.Mkdir(id, 0o755))
	_, excluded := backend.watchCreatedPath(id)
	assert.False(t, excluded)
	assert.Contains(t, backend.watcher.WatchList(), id)

	generated := filepath.Join(id, ".system_generated")
	require.NoError(t, os.Mkdir(generated, 0o755))
	_, excluded = backend.watchCreatedPath(generated)
	assert.False(t, excluded)
	assert.Contains(t, backend.watcher.WatchList(), generated)

	logs := filepath.Join(generated, "logs")
	require.NoError(t, os.Mkdir(logs, 0o755))
	_, excluded = backend.watchCreatedPath(logs)
	assert.False(t, excluded)
	assert.Contains(t, backend.watcher.WatchList(), logs)

	other := filepath.Join(generated, "other")
	require.NoError(t, os.Mkdir(other, 0o755))
	_, excluded = backend.watchCreatedPath(other)
	assert.True(t, excluded)
	assert.NotContains(t, backend.watcher.WatchList(), other)
}

func TestFSNotifyBackendAntigravityCLIContainerWatchPlan(t *testing.T) {
	backend := testFSNotifyBackend(t)
	container := filepath.Join(t.TempDir(), ".t3", "userdata", "providers", "antigravity")
	require.NoError(t, os.MkdirAll(container, 0o755))

	provider, ok := parser.NewProvider(parser.AgentAntigravityCLI, parser.ProviderConfig{
		Roots: []string{container},
	})
	require.True(t, ok)
	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)

	backend.setWatchRootPlan([]WatchRoot{{
		Path:             plan.Roots[0].Path,
		Recursive:        plan.Roots[0].Recursive,
		MaxDepth:         plan.Roots[0].MaxDepth,
		ExtraDirectories: plan.Roots[0].ExtraDirectories,
		Exists:           true,
	}})
	require.NoError(t, backend.AddRecursive(container, math.MaxInt).Err)

	// Runtime creation of a new T3 Code instance and its session directories
	// must be watched, while high-churn sibling trees stay excluded.
	const id = "66666666-7777-8888-9999-aaaaaaaaaaaa"
	instance := filepath.Join(container, "insthash")
	acp := filepath.Join(instance, "antigravity-acp")
	convDir := filepath.Join(acp, "conversations")
	brainDir := filepath.Join(acp, "brain")
	brainSession := filepath.Join(brainDir, id)
	generated := filepath.Join(brainSession, ".system_generated")
	logsDir := filepath.Join(generated, "logs")
	for _, dir := range []string{instance, acp, convDir, brainDir, brainSession, generated, logsDir} {
		require.NoError(t, os.Mkdir(dir, 0o755))
		_, excluded := backend.watchCreatedPath(dir)
		assert.False(t, excluded, "expected %s to be watched", dir)
		assert.Contains(t, backend.watcher.WatchList(), dir)
	}

	for _, ignored := range []string{
		filepath.Join(acp, "log"),
		filepath.Join(acp, "builtin"),
		filepath.Join(brainSession, "scratch"),
		filepath.Join(generated, "steps"),
		filepath.Join(logsDir, "chunks"),
	} {
		require.NoError(t, os.Mkdir(ignored, 0o755))
		_, excluded := backend.watchCreatedPath(ignored)
		assert.True(t, excluded, "expected %s to be excluded", ignored)
		assert.NotContains(t, backend.watcher.WatchList(), ignored)
	}

	// Session database and brain transcript events inside watched leaf dirs stay visible.
	for _, file := range []string{
		filepath.Join(convDir, id+".db-wal"),
		filepath.Join(logsDir, "transcript.jsonl"),
	} {
		event, relevant := backend.translateEvent(fsnotify.Event{
			Name: file, Op: fsnotify.Write,
		})
		assert.True(t, relevant, "expected event for %s to stay visible", file)
		assert.Equal(t, filepath.Clean(file), event.Path)
	}

	// Noise in intermediate directories stays ignored.
	_, relevant := backend.translateEvent(fsnotify.Event{
		Name: filepath.Join(generated, "noise.txt"), Op: fsnotify.Write,
	})
	assert.False(t, relevant)
}
