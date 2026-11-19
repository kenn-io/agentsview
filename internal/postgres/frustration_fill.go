package postgres

import (
	"context"
	"slices"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/poller"
)

// The fixture's 500-session chunks averaged 0.27 seconds. Five seconds leaves
// room for slower chunks while releasing requests blocked behind a writer.
const frustrationFillChunkTimeout = 5 * time.Second

type frustrationFillState struct {
	mu      sync.Mutex
	pending map[string]chan struct{}
}

func (s *Store) joinFrustrationFill(ctx context.Context, ids []string) error {
	state := &s.runtimeState().frustrationFill
	state.mu.Lock()
	channels := make(map[chan struct{}]struct{})
	for _, id := range ids {
		done, ok := state.pending[id]
		if !ok {
			state.mu.Unlock()
			return nil
		}
		channels[done] = struct{}{}
	}
	state.mu.Unlock()
	for done := range channels {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
		}
	}
	return nil
}

func (s *Store) fillFrustrationCounts(ctx context.Context, ids []string, column string) error {
	state := &s.runtimeState().frustrationFill
	chunks := make([][]string, 0, (len(ids)+maxPGVars-1)/maxPGVars)
	done := make([]chan struct{}, 0, cap(chunks))
	state.mu.Lock()
	if state.pending == nil {
		state.pending = make(map[string]chan struct{}, len(ids))
	}
	for chunk := range slices.Chunk(ids, maxPGVars) {
		ch := make(chan struct{})
		chunks = append(chunks, chunk)
		done = append(done, ch)
		for _, id := range chunk {
			state.pending[id] = ch
		}
	}
	state.mu.Unlock()
	release := func(i int) {
		state.mu.Lock()
		defer state.mu.Unlock()
		if len(chunks[i]) == 0 || state.pending[chunks[i][0]] != done[i] {
			return
		}
		for _, id := range chunks[i] {
			delete(state.pending, id)
		}
		close(done[i])
	}
	defer func() {
		for i := range chunks {
			release(i)
		}
	}()
	for i, chunk := range chunks {
		chunkCtx, cancel := context.WithTimeout(ctx, frustrationFillChunkTimeout)
		counts, versions, err := s.scanFrustrationCounts(chunkCtx, chunk, column)
		if err == nil {
			err = s.storeFrustrationCounts(chunkCtx, versions, counts, column)
		}
		cancel()
		release(i)
		if err != nil {
			return err
		}
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
