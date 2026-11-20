package postgres

import (
	"context"
	"log"
	"slices"
	"sync"
	"time"
)

// Bound a slow chunk so later chunks can still be prepared.
var frustrationFillChunkTimeout = 30 * time.Second

var frustrationJoinWait = 5 * time.Second

type frustrationChunk struct {
	ids      []string
	done     chan struct{}
	complete bool
}

type frustrationFillState struct {
	mu        sync.Mutex
	pending   map[string]*frustrationChunk
	chunks    []*frustrationChunk
	completed uint64
}

// release drops a finished chunk's session IDs and wakes waiters.
func (state *frustrationFillState) release(chunk *frustrationChunk) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if chunk.complete {
		return
	}
	for _, id := range chunk.ids {
		delete(state.pending, id)
	}
	chunk.ids = nil
	chunk.complete = true
	state.completed++
	close(chunk.done)
}

// joinFrustrationFill waits only for chunks fully covered by the request.
func (s *Store) joinFrustrationFill(ctx context.Context, ids []string, completed uint64) bool {
	state := &s.runtimeState().frustrationFill
	state.mu.Lock()
	wanted := make(map[*frustrationChunk]int)
	for _, id := range ids {
		chunk, ok := state.pending[id]
		if !ok {
			changed := state.completed != completed
			state.mu.Unlock()
			return changed
		}
		wanted[chunk]++
	}
	for chunk, count := range wanted {
		if count != len(chunk.ids) {
			state.mu.Unlock()
			return false
		}
	}
	var waits []<-chan struct{}
	for _, chunk := range state.chunks {
		if chunk.complete {
			continue
		}
		if wanted[chunk] == 0 {
			state.mu.Unlock()
			return false
		}
		waits = append(waits, chunk.done)
		if len(waits) == len(wanted) {
			break
		}
	}
	state.mu.Unlock()
	waitCtx, cancel := context.WithTimeout(ctx, frustrationJoinWait)
	defer cancel()
	for _, wait := range waits {
		select {
		case <-waitCtx.Done():
			return true
		case <-wait:
		}
	}
	return true
}

func (s *Store) fillFrustrationCounts(ctx context.Context, ids []string, column string) error {
	state := &s.runtimeState().frustrationFill
	state.mu.Lock()
	state.pending = make(map[string]*frustrationChunk, len(ids))
	state.chunks = nil
	for ids := range slices.Chunk(ids, maxPGVars) {
		chunk := &frustrationChunk{ids: ids, done: make(chan struct{})}
		state.chunks = append(state.chunks, chunk)
		for _, id := range ids {
			state.pending[id] = chunk
		}
	}
	chunks := state.chunks
	state.mu.Unlock()
	defer func() {
		for _, chunk := range chunks {
			state.release(chunk)
		}
	}()
	var firstErr error
	for _, chunk := range chunks {
		chunkCtx, cancel := context.WithTimeout(ctx, frustrationFillChunkTimeout)
		counts, versions, err := s.scanFrustrationCounts(chunkCtx, chunk.ids, column)
		if err == nil {
			err = s.storeFrustrationCounts(chunkCtx, versions, counts, column)
		}
		cancel()
		state.release(chunk)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// StartFrustrationFill prepares stale counts after the launch Usage warm-up.
func (s *Store) StartFrustrationFill(ctx context.Context) func() {
	if s.DB() == nil || s.hostedRelations {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if s.usageCache != nil {
			if err := s.usageCache.WaitWarm(ctx); err != nil {
				return
			}
		}
		writable, err := s.canFillFrustrationCounts(ctx)
		if err != nil {
			log.Printf("checking frustration count fill: %v", err)
			return
		}
		if !writable {
			return
		}
		column, err := sessionVersionColumnPG(ctx, s.pg)
		if err != nil {
			log.Printf("selecting frustration count version: %v", err)
			return
		}
		ids, err := s.staleFrustrationSessions(ctx, column)
		if err != nil {
			log.Printf("selecting stale frustration counts: %v", err)
			return
		}
		if err := s.fillFrustrationCounts(ctx, ids, column); err != nil && ctx.Err() == nil {
			log.Printf("filling frustration counts: %v", err)
		}
	}()
	return func() { cancel(); <-done }
}
