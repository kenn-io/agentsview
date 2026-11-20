package postgres

import (
	"context"
	"errors"
	"log"
	"slices"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// Bound a slow chunk so later chunks can still be prepared.
var frustrationFillChunkTimeout = 30 * time.Second

var frustrationJoinWait = 5 * time.Second

type frustrationChunk struct {
	ids  []string
	done chan struct{}
}

type frustrationFillState struct {
	mu        sync.Mutex
	pending   map[string]*frustrationChunk
	chunks    []*frustrationChunk
	completed uint64
}

type sharedLoadKey struct{}

var errSharedFillWait = errors.New("shared load cannot wait for count fill")

// sharedLoad marks a build detached from every caller's deadline.
func sharedLoad(ctx context.Context) context.Context {
	return context.WithValue(ctx, sharedLoadKey{}, true)
}

// keptSignals lets a caller whose shared load would wait on a pending count
// fill load under its own deadline instead.
func (s *Store) keptSignals(ctx context.Context, f db.AnalyticsFilter) (db.SignalsAnalyticsResponse, error) {
	result, err := keptAnalytics(ctx, s, "signals", f, s.loadAnalyticsSignals)
	if errors.Is(err, errSharedFillWait) {
		return s.loadAnalyticsSignals(ctx, f)
	}
	return result, err
}

// sharedFillWait reports whether a shared load needs counts a fill still owns.
func (s *Store) sharedFillWait(ctx context.Context, ids []string) bool {
	if ctx.Value(sharedLoadKey{}) == nil {
		return false
	}
	state := &s.runtimeState().frustrationFill
	state.mu.Lock()
	defer state.mu.Unlock()
	return slices.ContainsFunc(ids, func(id string) bool { return state.pending[id] != nil })
}

// completedCount is the number of finished chunks.
func (state *frustrationFillState) completedCount() uint64 {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.completed
}

// release drops a finished chunk's session IDs and wakes waiters.
func (state *frustrationFillState) release(chunk *frustrationChunk) {
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, id := range chunk.ids {
		delete(state.pending, id)
	}
	chunk.ids = nil
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
			if state.completed != completed {
				state.mu.Unlock()
				return true
			}
			continue
		}
		wanted[chunk]++
	}
	var waits []<-chan struct{}
	for _, chunk := range state.chunks {
		if len(chunk.ids) == 0 {
			continue
		}
		if wanted[chunk] != len(chunk.ids) {
			break
		}
		waits = append(waits, chunk.done)
	}
	state.mu.Unlock()
	if len(waits) == 0 {
		return false
	}
	budget := frustrationJoinWait
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)/2)
	}
	waitCtx, cancel := context.WithTimeout(ctx, budget)
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
	state.pending = make(map[string]*frustrationChunk)
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
	warmErr := s.WarmUsageCache(ctx)
	if warmErr != nil && !errors.Is(warmErr, db.ErrUsageSourceUnavailable) {
		log.Printf("pg serve: queueing Usage cache warm: %v", warmErr)
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if warmErr == nil && s.usageCache != nil {
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
