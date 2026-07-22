//go:build darwin && cgo

package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVnodeObserverWakesOnEntryCreation(t *testing.T) {
	dir := t.TempDir()
	woke := make(chan struct{}, 1)
	o, err := newVnodeObserver(func() {
		select {
		case woke <- struct{}{}:
		default:
		}
	})
	require.NoError(t, err)
	defer o.Close()
	require.NoError(t, o.Add(dir))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "child"), 0o755))
	select {
	case <-woke:
	case <-time.After(30 * time.Second):
		t.Fatal("no wake after directory entry creation")
	}
}

func TestVnodeObserverUsesOneDescriptorRegardlessOfEntries(t *testing.T) {
	dir := t.TempDir()
	for i := range 200 {
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, fmt.Sprintf("f%03d", i)), nil, 0o644))
	}
	o, err := newVnodeObserver(func() {})
	require.NoError(t, err)
	defer o.Close()
	require.NoError(t, o.Add(dir))
	assert.Equal(t, 1, o.watchedCount(),
		"observer must hold one descriptor per directory, not per entry")
}

// TestVnodeObserverCloseInterruptsBlockedRun proves Close synchronizes with the
// run loop: it writes the self-pipe wake so the blocked Kevent returns, Close
// waits for run() to exit, and no goroutine is left blocked on the kqueue.
func TestVnodeObserverCloseInterruptsBlockedRun(t *testing.T) {
	dir := t.TempDir()
	o, err := newVnodeObserver(func() {})
	require.NoError(t, err)
	require.NoError(t, o.Add(dir))

	closeDone := make(chan error, 1)
	go func() { closeDone <- o.Close() }()
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return; the blocked run loop was not interrupted")
	}
	select {
	case <-o.done:
	case <-time.After(time.Second):
		t.Fatal("run loop still active after Close returned")
	}
}

// TestVnodeObserverConcurrentCloseWaitsForTeardown proves a concurrent second
// Close does not return before the first finishes: every Close returns only
// after run() exited and the descriptors are torn down.
func TestVnodeObserverConcurrentCloseWaitsForTeardown(t *testing.T) {
	dir := t.TempDir()
	o, err := newVnodeObserver(func() {})
	require.NoError(t, err)
	require.NoError(t, o.Add(dir))

	const closers = 4
	results := make(chan error, closers)
	for range closers {
		go func() { results <- o.Close() }()
	}
	for range closers {
		select {
		case err := <-results:
			require.NoError(t, err)
			select {
			case <-o.done:
			default:
				t.Fatal("Close returned while the run loop was still active")
			}
			select {
			case <-o.closeDone:
			default:
				t.Fatal("Close returned before teardown completed")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent Close did not return")
		}
	}
	assert.Equal(t, 0, o.watchedCount())
}

func TestVnodeObserverRemoveAndCloseAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	o, err := newVnodeObserver(func() {})
	require.NoError(t, err)
	require.NoError(t, o.Add(dir))
	require.NoError(t, o.Remove(dir))
	require.NoError(t, o.Remove(dir)) // second remove is a no-op
	require.NoError(t, o.Close())
	require.NoError(t, o.Close())
}
