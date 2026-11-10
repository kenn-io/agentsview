//go:build darwin && cgo

package sync

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"go.kenn.io/agentsview/internal/fsevents"
)

// darwinShallowWatcher watches directories one level deep. FSEvents always
// reports a whole tree, so each root gets its own stream and the watcher
// drops events below the root's direct children. It reports the same events
// the shallow fsnotify watch did: creates, writes, removals, and renames of
// direct children, removal or rename of the root itself, and a full sync when
// FSEvents dropped events.
//
// FSEvents callbacks run on a dispatch queue and must never block, so they
// only append to a bounded pending list. A forwarding goroutine started by
// Start delivers the pending events on Events.
type darwinShallowWatcher struct {
	newStream func(string, func([]fsevents.Event)) (darwinStream, error)
	excludes  []string

	mu       sync.Mutex
	started  bool
	stopped  bool
	streams  map[string]darwinStream
	pending  []backendEvent
	overflow bool

	events chan backendEvent
	wake   chan struct{}
	stop   chan struct{}
	done   chan struct{}
}

// darwinShallowRoot maps a stream's native, symlink-resolved paths back to
// the logical root the caller registered.
type darwinShallowRoot struct {
	logicalPath string
	nativePath  string
}

func newDarwinShallowWatcher(
	excludes []string,
	newStream func(string, func([]fsevents.Event)) (darwinStream, error),
) *darwinShallowWatcher {
	return &darwinShallowWatcher{
		newStream: newStream,
		excludes:  excludes,
		streams:   make(map[string]darwinStream),
		events:    make(chan backendEvent),
		wake:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

func (w *darwinShallowWatcher) Events() <-chan backendEvent { return w.events }

// AddShallow starts watching root's direct children. Streams added before
// Start begin delivering when Start runs.
func (w *darwinShallowWatcher) AddShallow(root string) error {
	root = filepath.Clean(root)
	failed, err := w.addShallowLocked(root)
	if failed != nil {
		// Closing drains the shared callback queue, whose in-flight callback
		// may be waiting for w.mu, so close only after the lock is released.
		_ = failed.Close()
	}
	return err
}

// addShallowLocked registers root under w.mu. When the new stream fails to
// start it returns that stream for the caller to close outside the lock.
func (w *darwinShallowWatcher) addShallowLocked(root string) (darwinStream, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return nil, errors.New("shallow watcher is stopped")
	}
	if _, exists := w.streams[root]; exists {
		return nil, nil
	}
	registration := darwinShallowRoot{
		logicalPath: root,
		nativePath:  canonicalWatchRoot(root),
	}
	stream, err := w.newStream(registration.nativePath, func(events []fsevents.Event) {
		w.consume(registration, events)
	})
	if err != nil {
		return nil, fmt.Errorf("create shallow FSEvents stream for %s: %w", root, err)
	}
	if w.started {
		if err := stream.Start(); err != nil {
			return stream, fmt.Errorf("start shallow FSEvents stream for %s: %w", root, err)
		}
	}
	w.streams[root] = stream
	return nil, nil
}

// Remove stops watching root. Closing a stream waits for its in-flight
// callback, which takes w.mu, so the stream closes after the lock is released.
func (w *darwinShallowWatcher) Remove(root string) error {
	root = filepath.Clean(root)
	w.mu.Lock()
	stream, ok := w.streams[root]
	delete(w.streams, root)
	w.mu.Unlock()
	if !ok {
		return fmt.Errorf("no shallow watch for %s", root)
	}
	return stream.Close()
}

// Start begins delivery for every registered root. A failed stream start
// returns an error so the backend falls back to polling.
func (w *darwinShallowWatcher) Start() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return errors.New("shallow watcher is stopped")
	}
	if w.started {
		return nil
	}
	for root, stream := range w.streams {
		if err := stream.Start(); err != nil {
			return fmt.Errorf("start shallow FSEvents stream for %s: %w", root, err)
		}
	}
	w.started = true
	go w.forward()
	return nil
}

// Stop closes every stream and ends delivery. It returns after the forwarding
// goroutine has exited.
func (w *darwinShallowWatcher) Stop() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		<-w.done
		return
	}
	w.stopped = true
	started := w.started
	streams := make([]darwinStream, 0, len(w.streams))
	for _, stream := range w.streams {
		streams = append(streams, stream)
	}
	clear(w.streams)
	w.mu.Unlock()

	for _, stream := range streams {
		_ = stream.Close()
	}
	close(w.stop)
	if !started {
		close(w.events)
		close(w.done)
		return
	}
	<-w.done
}

func (w *darwinShallowWatcher) streamCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.streams)
}

// consume runs on the FSEvents dispatch queue. It translates and queues
// events without filesystem work or blocking.
func (w *darwinShallowWatcher) consume(root darwinShallowRoot, events []fsevents.Event) {
	if nativeDeliveryRequestsFallback(events) {
		w.queueFullSync()
		return
	}
	translated := make([]backendEvent, 0, len(events))
	for _, event := range events {
		path := logicalFSEventPath(darwinWatchRoot{
			logicalPath: root.logicalPath, nativePath: root.nativePath,
		}, event.Path)
		if converted, ok := translateShallowFSEvent(root.logicalPath, path, event.Flags); ok &&
			!shouldExcludeForRoot(w.excludes, converted.Path, root.logicalPath) {
			translated = append(translated, converted)
		}
	}
	if len(translated) == 0 {
		return
	}
	w.mu.Lock()
	if w.stopped || w.overflow {
		w.mu.Unlock()
		return
	}
	if len(w.pending)+len(translated) > defaultWatchBatchMaxEntries {
		w.pending = nil
		w.overflow = true
	} else {
		w.pending = append(w.pending, translated...)
	}
	w.mu.Unlock()
	w.signal()
}

func (w *darwinShallowWatcher) queueFullSync() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.pending = nil
	w.overflow = true
	w.mu.Unlock()
	w.signal()
}

func (w *darwinShallowWatcher) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *darwinShallowWatcher) forward() {
	defer close(w.done)
	defer close(w.events)
	for {
		select {
		case <-w.stop:
			return
		case <-w.wake:
		}
		w.mu.Lock()
		batch := w.pending
		if w.overflow {
			batch = []backendEvent{{Op: backendOpFullSync}}
		}
		w.pending = nil
		w.overflow = false
		w.mu.Unlock()
		for _, event := range batch {
			select {
			case w.events <- event:
			case <-w.stop:
				return
			}
		}
	}
}

// translateShallowFSEvent keeps events for root itself and its direct
// children. A change to the root directory's own contents is already
// reported through the child event, so only removal or rename of the root
// survives. FSEvents marks the root removed, renamed, or replaced with
// RootChanged, which maps to a root removal so the backend revalidates it.
func translateShallowFSEvent(
	root, path string,
	flags fsevents.EventFlags,
) (backendEvent, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if flags&fseventFlagRootChanged != 0 {
		return backendEvent{
			Path: root, Root: root, Op: backendOpRemove,
			ItemType: backendItemDirectory,
		}, true
	}
	if path == root {
		var op backendOp
		if flags&fseventFlagItemRemoved != 0 {
			op |= backendOpRemove
		}
		if flags&fseventFlagItemRenamed != 0 {
			op |= backendOpRename
		}
		if op == backendOpUnknown {
			return backendEvent{}, false
		}
		return backendEvent{
			Path: root, Root: root, Op: op, ItemType: backendItemDirectory,
		}, true
	}
	if filepath.Dir(path) != root {
		return backendEvent{}, false
	}
	return translateFSEvent(root, fsevents.Event{Path: path, Flags: flags})
}
