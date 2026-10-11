package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"
)

type codexThreadWatch struct {
	handle  windows.Handle
	info    os.FileInfo
	cleanup runtime.Cleanup
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
		handle, err := windows.FindFirstChangeNotification(codexThreadWatchPath(path), false,
			windows.FILE_NOTIFY_CHANGE_FILE_NAME|windows.FILE_NOTIFY_CHANGE_DIR_NAME)
		if err != nil {
			return &os.PathError{Op: "watch", Path: d.path, Err: err}
		}
		d.watch = codexThreadWatch{handle: handle, info: info}
		d.watch.cleanup = runtime.AddCleanup(d, func(handle windows.Handle) {
			_ = windows.FindCloseChangeNotification(handle)
		}, handle)
		return nil
	}
	status, err := windows.WaitForSingleObject(d.watch.handle, 0)
	if err == nil && status == uint32(windows.WAIT_TIMEOUT) {
		return nil
	}
	d.info = nil
	if err == nil && status != windows.WAIT_OBJECT_0 {
		err = fmt.Errorf("unexpected notification wait status %d", status)
	}
	if err == nil {
		// Rearm before enumeration so changes during the scan remain pending.
		err = windows.FindNextChangeNotification(d.watch.handle)
	}
	if err != nil {
		d.watch.close()
		return &os.PathError{Op: "watch", Path: d.path, Err: err}
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
