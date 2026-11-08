package sync

import (
	"runtime"
	"strings"
	gosync "sync"
	"sync/atomic"
	"time"
)

type syncLockOwner struct {
	operation uint64
	kind      syncOperationKind
	acquired  time.Time
}

type syncMutex struct {
	mu    gosync.Mutex
	owner atomic.Pointer[syncLockOwner]
}

func (m *syncMutex) Lock() {
	m.mu.Lock()
	m.registerOwner()
}

func (m *syncMutex) TryLock() bool {
	if !m.mu.TryLock() {
		return false
	}
	m.registerOwner()
	return true
}

func (m *syncMutex) Unlock() {
	// Clear before unlocking so this release cannot erase the next owner's registration.
	m.owner.Store(nil)
	m.mu.Unlock()
}

func (m *syncMutex) registerOwner() {
	acquired := time.Now()
	var pcs [8]uintptr
	n := runtime.Callers(3, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	kind := syncOperationKind("engine_lock")
	for {
		frame, more := frames.Next()
		helper := strings.HasSuffix(frame.Function, ".(*Engine).lockSync") ||
			strings.HasSuffix(frame.Function, ".(*Engine).lockSyncWithProgress")
		if frame.Function != "" && !helper {
			name := frame.Function
			if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
				name = name[slash+1:]
			}
			kind = syncOperationKind(name)
			break
		}
		if !more {
			break
		}
	}
	m.owner.Store(&syncLockOwner{
		operation: syncOperationSequence.Add(1), kind: kind, acquired: acquired,
	})
}
