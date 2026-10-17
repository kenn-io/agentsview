package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

type codexThreadWatch struct {
	handle  windows.Handle
	info    os.FileInfo
	cleanup runtime.Cleanup
}

// Windows signals a change notification handle from an APC queued to the
// thread that issued FindFirstChangeNotification or FindNextChangeNotification.
// Until that thread runs the APC, a wait from another thread times out even
// though the change has finished. A wait on the issuing thread runs its pending
// APCs first, so one locked thread issues and polls every notification. The
// thread lives for the rest of the process: exiting it would cancel pending
// notification requests.
var codexThreadWatcher struct {
	once     sync.Once
	mu       sync.Mutex
	requests chan codexThreadWatchRequest
	replies  chan codexThreadWatchReply
}

// A request with a path opens a watch; otherwise it polls and rearms handle.
type codexThreadWatchRequest struct {
	path   string
	handle windows.Handle
}

type codexThreadWatchReply struct {
	handle  windows.Handle
	changed bool
	err     error
}

func codexThreadWatchCall(request codexThreadWatchRequest) codexThreadWatchReply {
	w := &codexThreadWatcher
	w.once.Do(func() {
		w.requests = make(chan codexThreadWatchRequest)
		w.replies = make(chan codexThreadWatchReply)
		go func() {
			runtime.LockOSThread()
			for request := range w.requests {
				w.replies <- serveCodexThreadWatch(request)
			}
		}()
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	w.requests <- request
	return <-w.replies
}

func serveCodexThreadWatch(request codexThreadWatchRequest) codexThreadWatchReply {
	if request.path != "" {
		handle, err := windows.FindFirstChangeNotification(request.path, false,
			windows.FILE_NOTIFY_CHANGE_FILE_NAME|windows.FILE_NOTIFY_CHANGE_DIR_NAME)
		return codexThreadWatchReply{handle: handle, err: err}
	}
	status, err := windows.WaitForSingleObject(request.handle, 0)
	if err == nil && status == uint32(windows.WAIT_TIMEOUT) {
		return codexThreadWatchReply{}
	}
	if err == nil && status != windows.WAIT_OBJECT_0 {
		err = fmt.Errorf("unexpected notification wait status %d", status)
	}
	if err == nil {
		// Rearm before enumeration so changes during the scan remain pending.
		err = windows.FindNextChangeNotification(request.handle)
	}
	return codexThreadWatchReply{changed: true, err: err}
}

// Directory timestamps can stay unchanged after entries move on Windows.
// Watch each cached directory so a changed day does not rescan other days.
func (d *codexThreadDirectory) refreshWatch(info os.FileInfo) error {
	defer runtime.KeepAlive(d)
	if d.watch.handle != 0 && !os.SameFile(info, d.watch.info) {
		d.closeWatches()
	}
	if d.watch.handle == 0 {
		d.info = nil
		path, err := filepath.Abs(d.path)
		if err != nil {
			return err
		}
		reply := codexThreadWatchCall(codexThreadWatchRequest{path: codexThreadWatchPath(path)})
		if reply.err != nil {
			return &os.PathError{Op: "watch", Path: d.path, Err: reply.err}
		}
		d.watch = codexThreadWatch{handle: reply.handle, info: info}
		d.watch.cleanup = runtime.AddCleanup(d, func(handle windows.Handle) {
			_ = windows.FindCloseChangeNotification(handle)
		}, reply.handle)
		return nil
	}
	reply := codexThreadWatchCall(codexThreadWatchRequest{handle: d.watch.handle})
	if !reply.changed {
		return nil
	}
	d.info = nil
	if reply.err != nil {
		d.watch.close()
		return &os.PathError{Op: "watch", Path: d.path, Err: reply.err}
	}
	return nil
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
	if w.handle != 0 {
		w.cleanup.Stop()
		_ = windows.FindCloseChangeNotification(w.handle)
		*w = codexThreadWatch{}
	}
}
