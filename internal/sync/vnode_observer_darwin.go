//go:build darwin && cgo

package sync

import (
	"fmt"
	"path/filepath"
	gosync "sync"

	"golang.org/x/sys/unix"
)

// vnodeObserver watches directories for entry-level changes using one
// EVFILT_VNODE descriptor per directory. It exists for missing-root
// ancestors, where fsnotify's kqueue backend would open one descriptor per
// directory entry. Content changes to files inside the directory are
// deliberately out of scope: ancestors only need create/remove/rename of
// entries, and the lifecycle loop re-evaluates state on every wake.
type vnodeObserver struct {
	mu     gosync.Mutex
	kq     int
	fds    map[string]int
	paths  map[int]string
	wake   func()
	closed bool
	done   chan struct{}
	// wakeR/wakeW form a self-pipe registered with EVFILT_READ. Close writes a
	// byte to interrupt run()'s blocked Kevent: unlike closing the kqueue
	// mid-syscall, which Darwin does not guarantee to interrupt, a pipe write
	// always fires the read filter.
	wakeR int
	wakeW int
	// closeDone closes when the first Close finishes tearing down, so
	// concurrent Close calls wait for the teardown instead of returning while
	// descriptors are still live.
	closeDone chan struct{}
}

func newVnodeObserver(wake func()) (*vnodeObserver, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("create ancestor kqueue: %w", err)
	}
	var pipeFds [2]int
	if err := unix.Pipe(pipeFds[:]); err != nil {
		_ = unix.Close(kq)
		return nil, fmt.Errorf("create ancestor wake pipe: %w", err)
	}
	closeAll := func() {
		_ = unix.Close(pipeFds[0])
		_ = unix.Close(pipeFds[1])
		_ = unix.Close(kq)
	}
	for _, fd := range pipeFds {
		unix.CloseOnExec(fd)
	}
	// Nonblocking write end: Close's wake write must never block on a full
	// pipe; a pending byte already guarantees the wake.
	if err := unix.SetNonblock(pipeFds[1], true); err != nil {
		closeAll()
		return nil, fmt.Errorf("configure ancestor wake pipe: %w", err)
	}
	// Register the wake pipe before run() blocks so Close can interrupt the
	// blocked Kevent deterministically.
	shutdown := unix.Kevent_t{}
	unix.SetKevent(&shutdown, pipeFds[0], unix.EVFILT_READ, unix.EV_ADD)
	if _, err := unix.Kevent(kq, []unix.Kevent_t{shutdown}, nil, nil); err != nil {
		closeAll()
		return nil, fmt.Errorf("register ancestor shutdown event: %w", err)
	}
	o := &vnodeObserver{
		kq:        kq,
		fds:       make(map[string]int),
		paths:     make(map[int]string),
		wake:      wake,
		done:      make(chan struct{}),
		wakeR:     pipeFds[0],
		wakeW:     pipeFds[1],
		closeDone: make(chan struct{}),
	}
	go o.run()
	return o, nil
}

func (o *vnodeObserver) Add(path string) error {
	path = filepath.Clean(path)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return fmt.Errorf("vnode observer closed")
	}
	if _, exists := o.fds[path]; exists {
		return nil
	}
	fd, err := unix.Open(path, unix.O_EVTONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open ancestor %s: %w", path, err)
	}
	ev := unix.Kevent_t{}
	unix.SetKevent(&ev, fd, unix.EVFILT_VNODE, unix.EV_ADD|unix.EV_CLEAR)
	ev.Fflags = unix.NOTE_WRITE | unix.NOTE_DELETE |
		unix.NOTE_RENAME | unix.NOTE_REVOKE
	if _, err := unix.Kevent(o.kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("register ancestor %s: %w", path, err)
	}
	o.fds[path] = fd
	o.paths[fd] = path
	return nil
}

func (o *vnodeObserver) Remove(path string) error {
	path = filepath.Clean(path)
	o.mu.Lock()
	defer o.mu.Unlock()
	fd, exists := o.fds[path]
	if !exists {
		return nil
	}
	delete(o.fds, path)
	delete(o.paths, fd)
	// Closing the fd removes its kevent registration.
	return unix.Close(fd)
}

func (o *vnodeObserver) Close() error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		<-o.closeDone
		return nil
	}
	o.closed = true
	o.mu.Unlock()
	defer close(o.closeDone)

	// Wake run()'s blocked Kevent through the self-pipe, retrying interrupted
	// writes. EAGAIN means the pipe already holds an unread wake byte, so the
	// wake is guaranteed either way; a pipe write to a descriptor we own has
	// no other failure mode while the read end is open. Wait for run() to
	// return before closing the descriptors it reads, so no in-flight event
	// fires after shutdown.
	for {
		if _, err := unix.Write(o.wakeW, []byte{0}); err != unix.EINTR {
			break
		}
	}
	<-o.done

	o.mu.Lock()
	defer o.mu.Unlock()
	for path, fd := range o.fds {
		_ = unix.Close(fd)
		delete(o.fds, path)
		delete(o.paths, fd)
	}
	_ = unix.Close(o.wakeR)
	_ = unix.Close(o.wakeW)
	if err := unix.Close(o.kq); err != nil {
		return fmt.Errorf("closing ancestor kqueue: %w", err)
	}
	return nil
}

func (o *vnodeObserver) watchedCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.fds)
}

func (o *vnodeObserver) run() {
	defer close(o.done)
	events := make([]unix.Kevent_t, 8)
	for {
		n, err := unix.Kevent(o.kq, nil, events, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return // kq closed
		}
		shutdown := false
		vnodeEvents := false
		for i := range n {
			if events[i].Filter == unix.EVFILT_READ &&
				int(events[i].Ident) == o.wakeR {
				shutdown = true
			} else {
				vnodeEvents = true
			}
		}
		if vnodeEvents {
			o.wake()
		}
		if shutdown {
			return
		}
	}
}
