//go:build darwin && cgo

package fsevents

/*
#cgo darwin LDFLAGS: -framework CoreServices
#include "bridge_darwin.h"
*/
import "C"

import (
	"errors"
	"runtime"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	callbackMaxEvents    = 8192
	callbackMaxPathBytes = 2 << 20

	eventFlagMustScanSubDirs EventFlags = 0x00000001
	eventFlagUserDropped     EventFlags = 0x00000002
	eventFlagKernelDropped   EventFlags = 0x00000004
	eventFlagEventIDsWrapped EventFlags = 0x00000008
	eventFlagItemCreated     EventFlags = 0x00000100
)

var (
	errQueueUnavailable = errors.New("fsevents queue is unavailable")
	errStreamClosed     = errors.New("fsevents stream is closed")
)

// Event is one file-system change delivered by FSEvents.
type Event struct {
	Path  string
	ID    uint64
	Flags EventFlags
}

// EventFlags contains the native flags associated with an Event.
type EventFlags uint32

// Queue owns a shared serial dispatch queue used by one or more streams.
type Queue struct {
	mu     sync.Mutex
	native C.AVFSEventsQueueRef
	refs   uint64
}

// Stream delivers events for one root on a shared Queue.
type Stream struct {
	mu     sync.Mutex
	native C.AVFSEventsStreamRef
	queue  *Queue
	handle cgo.Handle
	sink   func([]Event)
	// callbackStats is an internal binding-boundary test observer. Production
	// streams leave it nil.
	callbackStats func(events, pathBytes int)
	started       bool
	stopping      atomic.Bool
	closeOnce     sync.Once
}

// NewQueue creates a shared serial dispatch queue for FSEvents callbacks.
func NewQueue() (*Queue, error) {
	native := C.avFSEventsQueueCreate()
	if native == nil {
		return nil, errors.New("create FSEvents dispatch queue")
	}
	queue := &Queue{native: native, refs: 1}
	runtime.SetFinalizer(queue, finalizeQueue)
	return queue, nil
}

// NewStream creates a stream for root and schedules it on q.
// The sink runs on q's serial callback queue and must not call Close
// synchronously.
func NewStream(q *Queue, root string, latency time.Duration, sink func([]Event)) (*Stream, error) {
	if q == nil {
		return nil, errors.New("fsevents queue is nil")
	}
	if root == "" {
		return nil, errors.New("fsevents root is empty")
	}
	if latency < 0 {
		return nil, errors.New("fsevents latency is negative")
	}
	if sink == nil {
		return nil, errors.New("fsevents sink is nil")
	}

	nativeQueue, err := q.retain()
	if err != nil {
		return nil, err
	}
	stream := &Stream{queue: q, sink: sink}
	stream.handle = cgo.NewHandle(stream)

	cRoot := C.CString(root)
	defer C.free(unsafe.Pointer(cRoot))
	stream.native = C.avFSEventsStreamCreate(
		nativeQueue,
		cRoot,
		C.double(latency.Seconds()),
		C.uintptr_t(stream.handle),
	)
	if stream.native == nil {
		stream.handle.Delete()
		q.release()
		return nil, errors.New("create FSEvents stream")
	}
	return stream, nil
}

// Start begins delivering events to the stream's sink.
func (s *Stream) Start() error {
	if s == nil {
		return errStreamClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping.Load() || s.native == nil {
		return errStreamClosed
	}
	if s.started {
		return nil
	}
	if !bool(C.avFSEventsStreamStart(s.native)) {
		return errors.New("start FSEvents stream")
	}
	s.started = true
	return nil
}

// Close stops delivery, drains callback ownership, and releases the stream.
func (s *Stream) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.stopping.Store(true)

		s.mu.Lock()
		native := s.native
		queue := s.queue
		started := s.started
		s.mu.Unlock()
		if native == nil {
			return
		}

		if started {
			C.avFSEventsStreamStop(native)
		}
		C.avFSEventsStreamInvalidate(native)
		queue.drain()
		C.avFSEventsStreamRelease(native)
		s.handle.Delete()
		queue.release()

		s.mu.Lock()
		s.native = nil
		s.queue = nil
		s.started = false
		s.handle = 0
		s.mu.Unlock()
	})
	return nil
}

func (q *Queue) retain() (C.AVFSEventsQueueRef, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.native == nil {
		return nil, errQueueUnavailable
	}
	C.avFSEventsQueueRetain(q.native)
	q.refs++
	return q.native, nil
}

func (q *Queue) release() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.native == nil || q.refs <= 1 {
		return
	}
	C.avFSEventsQueueRelease(q.native)
	q.refs--
}

func (q *Queue) drain() {
	q.mu.Lock()
	native := q.native
	q.mu.Unlock()
	if native != nil {
		C.avFSEventsQueueDrain(native)
	}
}

func finalizeQueue(q *Queue) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.native == nil {
		return
	}
	C.avFSEventsQueueRelease(q.native)
	q.native = nil
	q.refs = 0
}

//export goFSEventsCallback
func goFSEventsCallback(
	handle C.uintptr_t,
	count C.size_t,
	paths **C.char,
	flags *C.uint32_t,
	ids *C.uint64_t,
) {
	stream := cgo.Handle(handle).Value().(*Stream)
	if stream.stopping.Load() || count == 0 {
		return
	}
	if count > C.size_t(callbackMaxEvents) {
		stream.deliverCallback(
			[]Event{{Flags: eventFlagMustScanSubDirs}},
			0,
			0,
		)
		return
	}
	n := int(count)
	if n < 0 || C.size_t(n) != count {
		return
	}

	pathValues := unsafe.Slice(paths, n)
	flagValues := unsafe.Slice(flags, n)
	idValues := unsafe.Slice(ids, n)
	events := make([]Event, 0, n)
	copiedBytes := 0
	for i := range n {
		eventFlags := EventFlags(flagValues[i])
		if eventFlags&(eventFlagMustScanSubDirs|
			eventFlagUserDropped|
			eventFlagKernelDropped|
			eventFlagEventIDsWrapped) != 0 {
			stream.deliverCallback(
				[]Event{{Flags: eventFlagMustScanSubDirs}},
				len(events),
				copiedBytes,
			)
			return
		}
		remaining := callbackMaxPathBytes - copiedBytes
		if pathValues[i] == nil {
			stream.deliverCallback(
				[]Event{{Flags: eventFlagMustScanSubDirs}},
				len(events),
				copiedBytes,
			)
			return
		}
		pathLen := int(C.strnlen(pathValues[i], C.size_t(remaining+1)))
		if pathLen > remaining {
			stream.deliverCallback(
				[]Event{{Flags: eventFlagMustScanSubDirs}},
				len(events),
				copiedBytes,
			)
			return
		}
		events = append(events, Event{
			Path:  C.GoStringN(pathValues[i], C.int(pathLen)),
			ID:    uint64(idValues[i]),
			Flags: eventFlags,
		})
		copiedBytes += pathLen
	}
	stream.deliverCallback(events, len(events), copiedBytes)
}

func (s *Stream) deliverCallback(events []Event, copiedEvents, copiedBytes int) {
	if s.stopping.Load() {
		return
	}
	if s.callbackStats != nil {
		s.callbackStats(copiedEvents, copiedBytes)
	}
	s.sink(events)
}

func invokeTestCallback(stream *Stream, count int, path string, flags EventFlags) {
	handle := cgo.NewHandle(stream)
	defer handle.Delete()
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	C.avFSEventsInvokeTestCallback(
		C.uintptr_t(handle),
		C.size_t(count),
		cPath,
		C.uint32_t(flags),
	)
}
