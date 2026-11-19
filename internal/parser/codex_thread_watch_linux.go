package parser

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// One nonblocking notification descriptor serves the factory's cached roots.
// The inventory mutex protects the descriptor and all directory watch states.
type codexThreadWatchCache struct {
	set *codexThreadWatchSet
	// Nil boundaries use native syscalls; overrides stay local to this cache.
	initialize func(int) (int, error)
	addWatch   func(int, string, uint32) (int, error)
	readEvents func(int, []byte) (int, error)
}

type codexThreadWatchSet struct {
	fd      int
	states  map[int]*codexThreadWatchState
	cleanup runtime.Cleanup
}

type codexThreadWatchState struct {
	refs       int
	generation uint64
	removed    bool
}

type codexThreadWatch struct {
	set   *codexThreadWatchSet
	state *codexThreadWatchState
	info  os.FileInfo
	wd    int
	seen  uint64
}

func (c *codexThreadWatchCache) instance() (*codexThreadWatchSet, error) {
	if c.set != nil && c.set.fd >= 0 {
		return c.set, nil
	}
	initialize := unix.InotifyInit1
	if c.initialize != nil {
		initialize = c.initialize
	}
	fd, err := initialize(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	set := &codexThreadWatchSet{fd: fd, states: make(map[int]*codexThreadWatchState)}
	set.cleanup = runtime.AddCleanup(set, func(fd int) { _ = unix.Close(fd) }, fd)
	c.set = set
	return set, nil
}

// Linux can give successive directory changes the same mtime and ctime.
// Drain the kernel queue synchronously before using cached filenames.
func (c *codexThreadWatchCache) prepare() error {
	if c.set == nil || c.set.fd < 0 {
		return nil
	}
	read := unix.Read
	if c.readEvents != nil {
		read = c.readEvents
	}
	if err := c.set.poll(read); err != nil {
		// Overflow can hide removed watches too. Rebuild the instance on the
		// next lookup, rather than trusting permanently silent descriptors.
		c.set.close()
		return fmt.Errorf("read Codex directory notifications: %w", err)
	}
	return nil
}

func (d *codexThreadDirectory) refreshWatch(info os.FileInfo, cache *codexThreadWatchCache) error {
	w := &d.watch
	if w.state != nil && (w.set.fd < 0 || w.state.removed || !os.SameFile(info, w.info)) {
		w.close()
	}
	if w.state == nil {
		set, err := cache.instance()
		if err != nil {
			// Notifications are optional: enumerate readable directories on
			// every lookup until registration succeeds again.
			d.info = nil
			return nil
		}
		defer runtime.KeepAlive(set)
		addWatch := unix.InotifyAddWatch
		if cache.addWatch != nil {
			addWatch = cache.addWatch
		}
		wd, err := addWatch(set.fd, d.path, unix.IN_ONLYDIR|unix.IN_CREATE|unix.IN_DELETE|unix.IN_MOVED_FROM|unix.IN_MOVED_TO|unix.IN_DELETE_SELF|unix.IN_MOVE_SELF)
		if err != nil {
			if len(set.states) == 0 {
				set.close()
			}
			d.info = nil
			return nil
		}
		state := set.states[wd]
		if state == nil {
			state = &codexThreadWatchState{}
			set.states[wd] = state
		}
		state.refs++
		*w = codexThreadWatch{set: set, state: state, info: info, wd: wd, seen: state.generation}
		d.info = nil
	} else if w.seen != w.state.generation {
		w.seen = w.state.generation
		d.info = nil
	}
	return nil
}

func (s *codexThreadWatchSet) poll(read func(int, []byte) (int, error)) error {
	defer runtime.KeepAlive(s)
	// Bound this refresh to notifications already queued. A busy producer
	// must not keep a synchronous inventory lookup draining indefinitely.
	remaining, err := unix.IoctlGetInt(s.fd, unix.TIOCINQ)
	if err != nil {
		return err
	}
	var buffer [4096]byte
	for remaining > 0 {
		n, err := read(s.fd, buffer[:])
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) {
			return nil
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("directory notification descriptor closed")
		}
		remaining -= n
		for offset := 0; offset < n; {
			if n-offset < unix.SizeofInotifyEvent {
				return errors.New("incomplete directory notification")
			}
			header := buffer[offset : offset+unix.SizeofInotifyEvent]
			wd := int(int32(binary.NativeEndian.Uint32(header)))
			mask := binary.NativeEndian.Uint32(header[4:])
			next := offset + unix.SizeofInotifyEvent + int(binary.NativeEndian.Uint32(header[12:]))
			if next > n {
				return errors.New("incomplete directory notification name")
			}
			if mask&unix.IN_Q_OVERFLOW != 0 {
				return errors.New("directory notification queue overflowed")
			}
			if state := s.states[wd]; state != nil {
				state.generation++
				if mask&unix.IN_IGNORED != 0 {
					state.removed = true
					delete(s.states, wd)
				}
			}
			offset = next
		}
	}
	return nil
}

func (s *codexThreadWatchSet) close() {
	if s.fd >= 0 {
		s.cleanup.Stop()
		_ = unix.Close(s.fd)
		s.fd = -1
	}
}

func (w *codexThreadWatch) close() {
	if w.state == nil {
		return
	}
	s := w.set
	w.state.refs--
	if w.state.refs == 0 && s.states[w.wd] == w.state {
		delete(s.states, w.wd)
		if s.fd >= 0 && !w.state.removed {
			_, _ = unix.InotifyRmWatch(s.fd, uint32(w.wd))
		}
	}
	if len(s.states) == 0 {
		s.close()
	}
	*w = codexThreadWatch{}
}
