package postgres

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/poller"
)

// Bound a slow chunk so later chunks can still be prepared.
var frustrationFillChunkTimeout = 30 * time.Second

type frustrationChunk struct {
	ids       []string
	scanned   chan struct{}
	done      chan struct{}
	isScanned bool
	complete  bool
	counts    map[string]int
	versions  map[string]string
}

type frustrationFillState struct {
	mu      sync.Mutex
	pending map[string]*frustrationChunk
	chunks  []*frustrationChunk
}

// markScanned wakes requests waiting for the chunk's counts. Callers hold mu.
func (c *frustrationChunk) markScanned() {
	if !c.isScanned {
		c.isScanned = true
		close(c.scanned)
	}
}

// release drops a finished chunk's session IDs and results and wakes waiters.
func (state *frustrationFillState) release(chunk *frustrationChunk) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if chunk.complete {
		return
	}
	for _, id := range chunk.ids {
		delete(state.pending, id)
	}
	chunk.ids, chunk.counts, chunk.versions = nil, nil, nil
	chunk.complete = true
	chunk.markScanned()
	close(chunk.done)
}

// joinFrustrationFill waits for the active chunks that hold ids. A request
// that covers a whole chunk waits for its persistence. A filtered request
// waits only for the scan and reuses the result through the memo.
func (s *Store) joinFrustrationFill(ctx context.Context, ids []string, generation uint64) bool {
	state := &s.runtimeState().frustrationFill
	state.mu.Lock()
	wanted := make(map[*frustrationChunk]int)
	for _, id := range ids {
		chunk, ok := state.pending[id]
		if !ok {
			state.mu.Unlock()
			return false
		}
		wanted[chunk]++
	}
	var waits []*frustrationChunk
	filtered := make(map[*frustrationChunk]bool)
	for _, chunk := range state.chunks {
		if chunk.complete {
			continue
		}
		if wanted[chunk] == 0 {
			state.mu.Unlock()
			return false
		}
		filtered[chunk] = wanted[chunk] < len(chunk.ids)
		waits = append(waits, chunk)
		if len(waits) == len(wanted) {
			break
		}
	}
	state.mu.Unlock()
	waitCtx, cancel := halfDeadline(ctx)
	defer cancel()
	for _, chunk := range waits {
		wait := chunk.done
		if filtered[chunk] {
			wait = chunk.scanned
		}
		select {
		case <-waitCtx.Done():
			return true
		case <-wait:
		}
		if filtered[chunk] {
			s.memoizeFrustrationChunk(chunk, generation)
		}
	}
	return true
}

// halfDeadline leaves the rest of a request's time for its own scan.
func halfDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithTimeout(ctx, time.Until(deadline)/2)
	}
	return ctx, func() {}
}

// memoizeFrustrationChunk lets a filtered request reuse a chunk's scan.
func (s *Store) memoizeFrustrationChunk(chunk *frustrationChunk, generation uint64) {
	state := &s.runtimeState().frustrationFill
	state.mu.Lock()
	defer state.mu.Unlock()
	for id, version := range chunk.versions {
		s.runtimeState().frustrationMarkers.Store(generation, id, version, chunk.counts[id])
	}
}

// fillFrustrationChunk publishes a chunk's scan for filtered requests, then
// persists it.
func (s *Store) fillFrustrationChunk(ctx context.Context, chunk *frustrationChunk, column string) error {
	state := &s.runtimeState().frustrationFill
	counts, versions, err := s.scanFrustrationCounts(ctx, chunk.ids, column)
	state.mu.Lock()
	if err == nil {
		chunk.counts, chunk.versions = counts, versions
	}
	chunk.markScanned()
	state.mu.Unlock()
	if err != nil {
		return err
	}
	return s.storeFrustrationCounts(ctx, versions, counts, column)
}

func (s *Store) fillFrustrationCounts(ctx context.Context, ids []string, column string) error {
	state := &s.runtimeState().frustrationFill
	state.mu.Lock()
	if state.pending == nil {
		state.pending = make(map[string]*frustrationChunk, len(ids))
	}
	state.chunks = nil
	for ids := range slices.Chunk(ids, maxPGVars) {
		chunk := &frustrationChunk{ids: ids, scanned: make(chan struct{}), done: make(chan struct{})}
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
	var timedOut bool
	for _, chunk := range chunks {
		chunkCtx, cancel := context.WithTimeout(ctx, frustrationFillChunkTimeout)
		err := s.fillFrustrationChunk(chunkCtx, chunk, column)
		cancel()
		state.release(chunk)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			timedOut = true
		}
	}
	if timedOut {
		return context.DeadlineExceeded
	}
	return nil
}

// StartFrustrationFill prepares counts for writable replicas until stopped.
func (s *Store) StartFrustrationFill(ctx context.Context) func() {
	if s.DB() == nil || s.readTx != nil || s.hostedRelations {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	var revision int64
	var oid uint32
	scheduler := poller.Start(ctx, poller.Job{Name: "frustration counts", Interval: 5 * time.Second, RunAtStart: true, Run: func(ctx context.Context) error {
		writable, err := s.canFillFrustrationCounts(ctx)
		if err != nil || !writable {
			return err
		}
		currentOID, err := usageJournalOID(ctx, s.pg)
		if isUndefinedTable(err) || isInsufficientPrivilege(err) {
			return nil
		}
		if err != nil {
			return err
		}
		var current int64
		var changed []string
		if currentOID != oid {
			current, err = s.UsageChangeRevision(ctx)
		} else {
			current, changed, err = s.LoadUsageChanges(ctx, revision)
		}
		if err != nil || current == 0 {
			return err
		}
		if currentOID != oid || current < revision {
			changed = nil
		} else if len(changed) == 0 {
			revision, oid = current, currentOID
			return nil
		}
		column, err := sessionVersionColumnPG(ctx, s.pg)
		if err != nil {
			return err
		}
		ids, err := s.staleFrustrationSessions(ctx, column, changed)
		if err != nil {
			return err
		}
		if err := s.fillFrustrationCounts(ctx, ids, column); err != nil {
			return err
		}
		revision, oid = current, currentOID
		return nil
	}})
	return func() { cancel(); scheduler.Wait() }
}
