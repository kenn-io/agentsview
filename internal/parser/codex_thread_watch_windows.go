package parser

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type codexThreadWatch struct {
	state   *codexThreadWatchState
	info    os.FileInfo
	cleanup runtime.Cleanup
}

type codexThreadWatchCache struct{}

func (*codexThreadWatchCache) prepare() error { return nil }

// A watch keeps one asynchronous directory read pending. When the read
// completes, Windows queues it to a shared completion port, which any thread
// can drain. A change notification handle would stay unsignaled until the
// thread that requested it ran a queued callback.
//
// The kernel writes into overlapped and buffer until the pending read
// completes. codexThreadWatches.states keeps each state reachable until the
// port returns that read, including after the watch is closed.
type codexThreadWatchState struct {
	overlapped windows.Overlapped
	buffer     [64]uint32
	handle     windows.Handle
	pending    bool
	closed     bool
	changed    bool
	err        error
}

var codexThreadWatches struct {
	mu     sync.Mutex
	port   windows.Handle
	states map[*windows.Overlapped]*codexThreadWatchState
}

// Directory timestamps can stay unchanged after entries move on Windows.
// Watch each cached directory so a changed day does not rescan other days.
func (d *codexThreadDirectory) refreshWatch(info os.FileInfo, _ *codexThreadWatchCache) error {
	defer runtime.KeepAlive(d)
	if d.watch.state != nil && !os.SameFile(info, d.watch.info) {
		d.closeWatches()
	}
	if d.watch.state == nil {
		d.info = nil
		path, err := filepath.Abs(d.path)
		if err != nil {
			return err
		}
		state, err := openCodexThreadWatch(codexThreadWatchPath(path))
		if err != nil {
			return &os.PathError{Op: "watch", Path: d.path, Err: err}
		}
		d.watch = codexThreadWatch{state: state, info: info}
		d.watch.cleanup = runtime.AddCleanup(d, closeCodexThreadWatchState, state)
		return nil
	}
	changed, err := d.watch.state.poll()
	if changed || err != nil {
		d.info = nil
	}
	if err != nil {
		d.watch.close()
		return &os.PathError{Op: "watch", Path: d.path, Err: err}
	}
	return nil
}

func openCodexThreadWatch(path string) (*codexThreadWatchState, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, err
	}
	w := &codexThreadWatches
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.port == 0 {
		port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 0)
		if err != nil {
			_ = windows.CloseHandle(handle)
			return nil, fmt.Errorf("create completion port: %w", err)
		}
		w.port = port
		w.states = make(map[*windows.Overlapped]*codexThreadWatchState)
	}
	// A cache rebuilt after invalidation only opens watches, so polls alone
	// would never reclaim the watches the invalidation closed.
	if err := drainCodexThreadWatches(); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if _, err := windows.CreateIoCompletionPort(handle, w.port, 0, 0); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	state := &codexThreadWatchState{handle: handle}
	if err := state.read(); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	w.states[&state.overlapped] = state
	return state, nil
}

// read starts the next directory read. Callers hold codexThreadWatches.mu.
func (s *codexThreadWatchState) read() error {
	s.overlapped = windows.Overlapped{}
	err := windows.ReadDirectoryChanges(s.handle, (*byte)(unsafe.Pointer(&s.buffer[0])), uint32(unsafe.Sizeof(s.buffer)),
		false, windows.FILE_NOTIFY_CHANGE_FILE_NAME|windows.FILE_NOTIFY_CHANGE_DIR_NAME, nil, &s.overlapped, 0)
	s.pending = err == nil
	return err
}

// poll reports whether the directory changed since the last poll. It drains
// every completed read, since one port serves all watches.
func (s *codexThreadWatchState) poll() (bool, error) {
	w := &codexThreadWatches
	w.mu.Lock()
	defer w.mu.Unlock()
	drainErr := drainCodexThreadWatches()
	changed := s.changed
	s.changed = false
	return changed, cmp.Or(s.err, drainErr)
}

// drainCodexThreadWatches handles completed reads. Callers hold
// codexThreadWatches.mu.
func drainCodexThreadWatches() error {
	w := &codexThreadWatches
	for {
		var bytes uint32
		var key uintptr
		var overlapped *windows.Overlapped
		err := windows.GetQueuedCompletionStatus(w.port, &bytes, &key, &overlapped, 0)
		if overlapped == nil {
			if errors.Is(err, windows.WAIT_TIMEOUT) {
				return nil
			}
			return fmt.Errorf("read completion port: %w", err)
		}
		state := w.states[overlapped]
		if state == nil {
			return errors.New("completion for an unknown directory watch")
		}
		state.pending = false
		if state.closed {
			delete(w.states, overlapped)
			continue
		}
		// Any completion means the listing may differ. Zero bytes and
		// ERROR_NOTIFY_ENUM_DIR mean the kernel dropped details, which a
		// rescan covers too.
		state.changed = true
		if err != nil && !errors.Is(err, windows.ERROR_NOTIFY_ENUM_DIR) {
			state.err = err
			continue
		}
		if err := state.read(); err != nil {
			state.err = err
		}
	}
}

// closeCodexThreadWatchState closes the directory handle. Closing cancels a
// pending read; the state stays registered until the port returns it.
func closeCodexThreadWatchState(s *codexThreadWatchState) {
	w := &codexThreadWatches
	w.mu.Lock()
	defer w.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	_ = windows.CloseHandle(s.handle)
	if !s.pending {
		delete(w.states, &s.overlapped)
	}
}

// The raw Win32 call does not extend long paths as Go's file APIs do.
// Keep already-qualified paths intact; path must be absolute.
func codexThreadWatchPath(path string) string {
	if len(path) < 248 || strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) || strings.HasPrefix(path, `\??\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}

func (w *codexThreadWatch) close() {
	if w.state != nil {
		w.cleanup.Stop()
		closeCodexThreadWatchState(w.state)
		*w = codexThreadWatch{}
	}
}
