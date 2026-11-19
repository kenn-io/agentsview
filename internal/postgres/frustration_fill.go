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
	ids      []string
	done     chan struct{}
	complete bool
}

type frustrationFillState struct {
	mu      sync.Mutex
	pending map[string]*frustrationChunk
	chunks  []*frustrationChunk
}

func (s *Store) joinFrustrationFill(ctx context.Context, ids []string) bool {
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
	for chunk, count := range wanted {
		if count != len(chunk.ids) {
			state.mu.Unlock()
			return false
		}
	}
	channels := make([]<-chan struct{}, 0, len(wanted))
	for _, chunk := range state.chunks {
		if chunk.complete {
			continue
		}
		if _, ok := wanted[chunk]; !ok {
			state.mu.Unlock()
			return false
		}
		channels = append(channels, chunk.done)
		if len(channels) == len(wanted) {
			break
		}
	}
	state.mu.Unlock()
	waitCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, time.Until(deadline)/2)
		defer cancel()
	}
	for _, done := range channels {
		select {
		case <-waitCtx.Done():
			return true
		case <-done:
		}
	}
	return true
}

func (s *Store) fillFrustrationCounts(ctx context.Context, ids []string, column string) error {
	state := &s.runtimeState().frustrationFill
	state.mu.Lock()
	if state.pending == nil {
		state.pending = make(map[string]*frustrationChunk, len(ids))
	}
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
	release := func(chunk *frustrationChunk) {
		state.mu.Lock()
		defer state.mu.Unlock()
		if chunk.complete {
			return
		}
		for _, id := range chunk.ids {
			delete(state.pending, id)
		}
		chunk.complete = true
		close(chunk.done)
	}
	defer func() {
		for _, chunk := range chunks {
			release(chunk)
		}
	}()
	var timedOut bool
	for _, chunk := range chunks {
		chunkCtx, cancel := context.WithTimeout(ctx, frustrationFillChunkTimeout)
		counts, versions, err := s.scanFrustrationCounts(chunkCtx, chunk.ids, column)
		if err == nil {
			err = s.storeFrustrationCounts(chunkCtx, versions, counts, column)
		}
		cancel()
		release(chunk)
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
		current, changed, err := s.LoadUsageChanges(ctx, revision)
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
