package db

import (
	"reflect"
	"sync"

	"go.kenn.io/agentsview/internal/db/bunmodel"
)

// writeSlicePool reuses fixed row backing across serialized archive writes.
// Dynamic payload references are cleared before a lease is returned. The
// existing canonical payload budget also bounds fixed backing retention, so a
// single unusually large transcript cannot pin an unbounded slice in memory.
type writeSlicePool[T any] struct {
	pool sync.Pool
}

type writeSliceLease[T any] struct {
	rows []T
}

func (p *writeSlicePool[T]) acquire(length int) *writeSliceLease[T] {
	lease, _ := p.pool.Get().(*writeSliceLease[T])
	if lease == nil {
		lease = &writeSliceLease[T]{}
	}
	if cap(lease.rows) < length {
		lease.rows = make([]T, length)
	} else {
		lease.rows = lease.rows[:length]
	}
	return lease
}

func (p *writeSlicePool[T]) release(lease *writeSliceLease[T]) {
	if lease == nil {
		return
	}
	rowBytes := reflect.TypeFor[T]().Size()
	if rowBytes > 0 &&
		uintptr(cap(lease.rows)) > uintptr(canonicalWriteBatchPayloadLimit)/rowBytes {
		lease.rows = nil
	} else {
		clear(lease.rows[:cap(lease.rows)])
		lease.rows = lease.rows[:0]
	}
	p.pool.Put(lease)
}

var (
	sanitizedMessagePool  writeSlicePool[Message]
	sanitizedUsagePool    writeSlicePool[UsageEvent]
	archiveMessageRowPool writeSlicePool[bunmodel.Message]
)

type sessionBatchSanitization struct {
	messages *writeSliceLease[Message]
	usage    *writeSliceLease[UsageEvent]
}

func (s *sessionBatchSanitization) release() {
	sanitizedMessagePool.release(s.messages)
	sanitizedUsagePool.release(s.usage)
	s.messages = nil
	s.usage = nil
}
