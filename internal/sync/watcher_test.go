package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startTestWatcherNoCleanup sets up a watcher without registering
// t.Cleanup(w.Stop), for tests that explicitly exercise Stop().
func startTestWatcherNoCleanup(
	t *testing.T, onChange func([]string), debounce time.Duration,
) (*Watcher, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := NewWatcher(debounce, onChange, nil)
	require.NoError(t, err, "NewWatcher")
	_, _, err = w.WatchRecursive(dir)
	require.NoError(t, err, "WatchRecursive")
	w.Start()
	return w, dir
}

// startTestWatcher encapsulates watcher setup and lifecycle.
func startTestWatcher(
	t *testing.T, onChange func([]string),
) (*Watcher, string) {
	t.Helper()
	w, dir := startTestWatcherNoCleanup(t, onChange, 50*time.Millisecond)
	t.Cleanup(func() { w.Stop() })
	return w, dir
}

// pollUntil dynamically polls a condition to avoid hardcoded sleeps.
func pollUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pollUntil: condition not met within deadline")
}

func TestWatcherCallsOnChange(t *testing.T) {
	pathsCh := make(chan []string, 1)

	_, dir := startTestWatcher(t, func(paths []string) {
		select {
		case pathsCh <- paths:
		default:
		}
	})

	path := filepath.Join(dir, "test.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))

	select {
	case gotPaths := <-pathsCh:
		require.NotEmpty(t, gotPaths, "onChange called with empty paths")
		require.True(t, slices.Contains(gotPaths, path),
			"onChange did not contain expected path %s, got %v", path, gotPaths)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for onChange callback")
	}
}

func TestWatcherAutoWatchesNewDirs(t *testing.T) {
	pathsCh := make(chan []string, 10)

	w, dir := startTestWatcher(t, func(paths []string) {
		pathsCh <- paths
	})

	subdir := filepath.Join(dir, "newdir")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	// Wait for fsnotify to process the mkdir and add the watch
	pollUntil(t, func() bool {
		return slices.Contains(w.watcher.WatchList(), subdir)
	})

	nestedPath := filepath.Join(subdir, "nested.jsonl")
	require.NoError(t, os.WriteFile(nestedPath, []byte("nested"), 0o644))

	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		select {
		case paths := <-pathsCh:
			if slices.Contains(paths, nestedPath) {
				found = true
			}
		case <-time.After(50 * time.Millisecond):
		}
	}

	require.True(t, found, "timed out waiting for nested file change")
}

func TestWatcherStopIsClean(t *testing.T) {
	w, _ := startTestWatcherNoCleanup(t, func(_ []string) {}, 50*time.Millisecond)

	stopped := make(chan struct{})
	go func() {
		w.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return in time")
	}
}

func TestWatcherStopIdempotency(t *testing.T) {
	w, _ := startTestWatcherNoCleanup(t, func(_ []string) {}, 50*time.Millisecond)

	// 1. Sequential double stop
	w.Stop()
	w.Stop()

	// 2. Concurrent stop attempts
	pathsCh2 := make(chan []string, 10)
	w2, dir2 := startTestWatcherNoCleanup(
		t, func(paths []string) {
			pathsCh2 <- paths
		}, 50*time.Millisecond,
	)

	stressPath := filepath.Join(dir2, "stress.txt")
	require.NoError(t, os.WriteFile(stressPath, []byte("data"), 0o644), "stress write")

	// Wait for fsnotify to process it before concurrent stop
	select {
	case <-pathsCh2:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stress file to be processed")
	}

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			w2.Stop()
		})
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Stop() timed out")
	}
}

func TestWatcherIgnoresNonWriteCreate(t *testing.T) {
	pathsCh := make(chan []string, 10)
	w, dir := startTestWatcherNoCleanup(t, func(paths []string) {
		pathsCh <- paths
	}, 10*time.Millisecond)
	t.Cleanup(func() { w.Stop() })

	path := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	// Wait for the initial write event to clear
	select {
	case <-pathsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial write event")
	}

	// Now do a chmod (should be ignored)
	require.NoError(t, os.Chmod(path, 0o666))

	// We can manually flush and see if anything triggers, but since the
	// event won't even be recorded, flush won't do anything. We just wait a bit.
	select {
	case <-pathsCh:
		t.Fatal("onChange called for chmod event, expected it to be ignored")
	case <-time.After(100 * time.Millisecond):
		// Success
	}
}

func TestWatcherHandlesRemoveAndRename(t *testing.T) {
	pathsCh := make(chan []string, 1)
	w, err := NewWatcher(time.Millisecond, func(paths []string) {
		pathsCh <- paths
	}, nil)
	require.NoError(t, err, "NewWatcher")
	w.Start()
	t.Cleanup(func() { w.Stop() })
	base := time.Unix(0, 0)
	w.now = func() time.Time { return base }

	w.handleEvent(fsnotify.Event{
		Name: "/tmp/remove.json",
		Op:   fsnotify.Remove,
	})
	w.handleEvent(fsnotify.Event{
		Name: "/tmp/rename.json",
		Op:   fsnotify.Rename,
	})
	w.now = func() time.Time { return base.Add(2 * time.Millisecond) }
	w.flush()

	got := <-pathsCh
	assert.Contains(t, got, "/tmp/remove.json")
	assert.Contains(t, got, "/tmp/rename.json")
}

func TestWatcherDebounceLogic(t *testing.T) {
	var mu sync.Mutex
	mockTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	pathsCh := make(chan []string, 1)

	// Use a long debounce so the internal ticker doesn't trigger naturally during the test
	w, dir := startTestWatcherNoCleanup(t, func(paths []string) {
		select {
		case pathsCh <- paths:
		default:
		}
	}, 1*time.Hour)
	t.Cleanup(func() { w.Stop() })

	w.mu.Lock()
	w.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return mockTime
	}
	w.mu.Unlock()

	path := filepath.Join(dir, "recent_dir")
	require.NoError(t, os.Mkdir(path, 0o755))

	// Wait for fsnotify to process the mkdir and add the watch
	pollUntil(t, func() bool {
		return slices.Contains(w.watcher.WatchList(), path)
	})

	// 1. Flush before debounce period
	w.flush()
	select {
	case <-pathsCh:
		t.Fatal("flush should not call onChange before debounce")
	default:
	}

	// 2. Advance time past debounce period
	mu.Lock()
	mockTime = mockTime.Add(2 * time.Hour)
	mu.Unlock()

	// 3. Flush after debounce period
	w.flush()

	select {
	case gotPaths := <-pathsCh:
		require.Len(t, gotPaths, 1, "expected [%s], got %v", path, gotPaths)
		assert.Equal(t, path, gotPaths[0])
	case <-time.After(5 * time.Second):
		t.Fatal("expected onChange to be called after debounce elapsed")
	}

	// 4. Flush again when empty should be a no-op
	w.flush()
	select {
	case <-pathsCh:
		t.Fatal("flush should not call onChange when empty")
	default:
	}
}

func TestWatchRecursive_ExcludesDirectoryNames(t *testing.T) {
	w, err := NewWatcher(time.Second, func(_ []string) {}, []string{".git", "node_modules"})
	require.NoError(t, err, "NewWatcher")
	w.Start()
	t.Cleanup(func() { w.Stop() })

	root := t.TempDir()
	included := filepath.Join(root, "project", "src")
	excludedGit := filepath.Join(root, "project", ".git", "objects")
	excludedModules := filepath.Join(root, "project", "node_modules", "pkg")
	for _, p := range []string{included, excludedGit, excludedModules} {
		require.NoError(t, os.MkdirAll(p, 0o755), "MkdirAll(%s)", p)
	}

	_, _, err = w.WatchRecursive(root)
	require.NoError(t, err, "WatchRecursive")

	got := w.watcher.WatchList()
	assert.NotContains(t, got, filepath.Join(root, "project", ".git"),
		".git should be excluded from watch list")
	assert.NotContains(t, got, filepath.Join(root, "project", "node_modules"),
		"node_modules should be excluded from watch list")
	assert.Contains(t, got, included, "expected included dir in watch list")
}

func TestWatchRecursiveBudget_DegradesWhenBudgetExhausted(t *testing.T) {
	root := t.TempDir()
	for i := range 5 {
		require.NoError(t, os.MkdirAll(filepath.Join(root, fmt.Sprintf("dir-%d", i)), 0o755))
	}

	w, err := NewWatcher(time.Second, func(_ []string) {}, nil)
	require.NoError(t, err, "NewWatcher")
	w.Start()
	t.Cleanup(func() { w.Stop() })

	result := w.WatchRecursiveBudgeted(root, 3)
	assert.Equal(t, 3, result.Watched)
	assert.True(t, result.BudgetExhausted, "BudgetExhausted = false, want true")
}

func TestIsWatchResourceExhaustion(t *testing.T) {
	assert.True(t, isWatchResourceExhaustion(syscall.EMFILE), "EMFILE should be resource exhaustion")
	assert.True(t, isWatchResourceExhaustion(syscall.ENOSPC), "ENOSPC should be resource exhaustion")
	assert.False(t, isWatchResourceExhaustion(os.ErrNotExist), "ErrNotExist should not be resource exhaustion")
}

func TestWatcherAutoWatchesNewDirs_RespectsExcludes(t *testing.T) {
	pathsCh := make(chan []string, 10)
	w, err := NewWatcher(20*time.Millisecond, func(paths []string) {
		pathsCh <- paths
	}, []string{".git"})
	require.NoError(t, err, "NewWatcher")
	t.Cleanup(func() { w.Stop() })

	root := t.TempDir()
	_, _, err = w.WatchRecursive(root)
	require.NoError(t, err, "WatchRecursive")
	w.Start()

	gitDir := filepath.Join(root, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755), "Mkdir(.git)")

	time.Sleep(100 * time.Millisecond)
	assert.NotContains(t, w.watcher.WatchList(), gitDir,
		"newly created excluded dir should not be watched")

	fileInGit := filepath.Join(gitDir, "config")
	require.NoError(t, os.WriteFile(fileInGit, []byte("x"), 0o644))

	select {
	case paths := <-pathsCh:
		assert.NotContains(t, paths, fileInGit,
			"changes inside excluded dir should not trigger onChange")
	case <-time.After(200 * time.Millisecond):
		// no events from excluded dir; expected
	}
}

func TestWatcherShallowRootDoesNotAutoWatchNewDirs(t *testing.T) {
	pathsCh := make(chan []string, 10)
	w, err := NewWatcher(20*time.Millisecond, func(paths []string) {
		pathsCh <- paths
	}, nil)
	require.NoError(t, err, "NewWatcher")
	t.Cleanup(func() { w.Stop() })

	root := t.TempDir()
	require.True(t, w.WatchShallow(root), "WatchShallow")
	w.Start()

	localDir := filepath.Join(root, "local_session")
	require.NoError(t, os.Mkdir(localDir, 0o755), "Mkdir(local)")

	select {
	case paths := <-pathsCh:
		assert.Contains(t, paths, localDir,
			"root-level create should still trigger onChange")
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for shallow root create event")
	}
	assert.NotContains(t, w.watcher.WatchList(), localDir,
		"shallow root descendant should not be auto-watched")
}

// A shallow parent root (e.g. Codex's .codex) must not shadow a recursive
// child root (.codex/sessions) nested inside it: newly created directories
// under the recursive child still need to be auto-watched so new sessions in
// new date directories live-sync.
func TestWatcherShallowParentDoesNotShadowRecursiveChild(t *testing.T) {
	pathsCh := make(chan []string, 10)
	w, err := NewWatcher(20*time.Millisecond, func(paths []string) {
		pathsCh <- paths
	}, nil)
	require.NoError(t, err, "NewWatcher")
	t.Cleanup(func() { w.Stop() })

	parent := t.TempDir()
	child := filepath.Join(parent, "sessions")
	require.NoError(t, os.Mkdir(child, 0o755), "Mkdir(child)")

	require.True(t, w.WatchShallow(parent), "WatchShallow(parent)")
	_, _, err = w.WatchRecursive(child)
	require.NoError(t, err, "WatchRecursive(child)")
	w.Start()

	// A sibling write directly under the shallow parent is still seen but its
	// directory is not auto-watched.
	logDir := filepath.Join(parent, "log")
	require.NoError(t, os.Mkdir(logDir, 0o755), "Mkdir(log)")

	// A new directory created under the recursive child must be auto-watched
	// even though it also sits inside the shallow parent root.
	dateDir := filepath.Join(child, "2026-06-16")
	require.NoError(t, os.Mkdir(dateDir, 0o755), "Mkdir(dateDir)")
	pollUntil(t, func() bool {
		return slices.Contains(w.watcher.WatchList(), dateDir)
	})

	sessionFile := filepath.Join(dateDir, "rollout.jsonl")
	require.NoError(t, os.WriteFile(sessionFile, []byte("x"), 0o644))

	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		select {
		case paths := <-pathsCh:
			if slices.Contains(paths, sessionFile) {
				found = true
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	require.True(t, found,
		"file in a new date dir under the recursive child must trigger onChange")
	assert.NotContains(t, w.watcher.WatchList(), logDir,
		"sibling dir under the shallow parent must not be auto-watched")
}

func TestWatchRecursive_RootUnderExcludedAncestorStillWatchesDescendants(t *testing.T) {
	w, err := NewWatcher(time.Second, func(_ []string) {}, []string{"venv"})
	require.NoError(t, err, "NewWatcher")
	w.Start()
	t.Cleanup(func() { w.Stop() })

	base := t.TempDir()
	root := filepath.Join(base, "venv", "project")
	included := filepath.Join(root, "src")
	require.NoError(t, os.MkdirAll(included, 0o755), "MkdirAll(%s)", included)

	_, _, err = w.WatchRecursive(root)
	require.NoError(t, err, "WatchRecursive")

	got := w.watcher.WatchList()
	assert.Contains(t, got, root, "expected root in watch list")
	assert.Contains(t, got, included, "expected included dir in watch list")
}

func TestWatchRecursive_ExcludesSlashPatternRelativeToRoot(t *testing.T) {
	w, err := NewWatcher(time.Second, func(_ []string) {}, []string{"foo/bar"})
	require.NoError(t, err, "NewWatcher")
	w.Start()
	t.Cleanup(func() { w.Stop() })

	root := t.TempDir()
	excluded := filepath.Join(root, "foo", "bar")
	includedSibling := filepath.Join(root, "foo", "baz")
	for _, p := range []string{excluded, includedSibling} {
		require.NoError(t, os.MkdirAll(p, 0o755), "MkdirAll(%s)", p)
	}

	_, _, err = w.WatchRecursive(root)
	require.NoError(t, err, "WatchRecursive")

	got := w.watcher.WatchList()
	assert.NotContains(t, got, excluded, "expected %s to be excluded", excluded)
	assert.Contains(t, got, includedSibling, "expected %s to be included", includedSibling)
}

func TestWatchRecursive_OverlappingRoots_UsesMostSpecificRoot(t *testing.T) {
	w, err := NewWatcher(time.Second, func(_ []string) {}, []string{"venv"})
	require.NoError(t, err, "NewWatcher")
	w.Start()
	t.Cleanup(func() { w.Stop() })

	base := t.TempDir()
	parentRoot := filepath.Join(base, "workspace")
	nestedRoot := filepath.Join(parentRoot, "venv", "project")
	included := filepath.Join(nestedRoot, "src")
	for _, p := range []string{parentRoot, included} {
		require.NoError(t, os.MkdirAll(p, 0o755), "MkdirAll(%s)", p)
	}

	_, _, err = w.WatchRecursive(parentRoot)
	require.NoError(t, err, "WatchRecursive(parent)")
	_, _, err = w.WatchRecursive(nestedRoot)
	require.NoError(t, err, "WatchRecursive(nested)")

	got := w.watcher.WatchList()
	assert.Contains(t, got, nestedRoot, "expected nested root in watch list")
	assert.Contains(t, got, included, "expected included dir in watch list")
}

func TestWatcherExcludedCreateDir_DoesNotTriggerOnChange(t *testing.T) {
	pathsCh := make(chan []string, 10)
	w, err := NewWatcher(20*time.Millisecond, func(paths []string) {
		pathsCh <- paths
	}, []string{".git"})
	require.NoError(t, err, "NewWatcher")
	t.Cleanup(func() { w.Stop() })

	root := t.TempDir()
	_, _, err = w.WatchRecursive(root)
	require.NoError(t, err, "WatchRecursive")
	w.Start()

	gitDir := filepath.Join(root, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755), "Mkdir(.git)")

	select {
	case paths := <-pathsCh:
		assert.NotContains(t, paths, gitDir,
			"excluded directory create should not trigger onChange")
	case <-time.After(250 * time.Millisecond):
		// Expected: no callback for excluded dir creation.
	}
}

func TestNewWatcher_NilOnChange(t *testing.T) {
	_, err := NewWatcher(time.Second, nil, nil)
	require.Error(t, err, "NewWatcher(nil) should return error")
	require.ErrorIs(t, err, os.ErrInvalid)

	expectedMsg := "onChange callback is nil"
	assert.Equal(t, expectedMsg+": "+os.ErrInvalid.Error(), err.Error())
}
