package postgres

import (
	"context"
	"math"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/poller"
)

type frustrationBatch struct {
	ids  map[string]struct{}
	done chan struct{}
}

type frustrationFillState struct {
	mu     sync.Mutex
	active *frustrationBatch
}

func (s *Store) frustrationFillDone(ids []string) <-chan struct{} {
	state := &s.runtimeState().frustrationFill
	state.mu.Lock()
	defer state.mu.Unlock()
	batch := state.active
	if batch == nil {
		return nil
	}
	for _, id := range ids {
		if _, ok := batch.ids[id]; !ok {
			return nil
		}
	}
	return batch.done
}

func (s *Store) fillFrustrationCounts(ctx context.Context, ids []string, column string) error {
	return pgQueryChunked(ids, func(ids []string) error {
		batch := &frustrationBatch{ids: make(map[string]struct{}, len(ids)), done: make(chan struct{})}
		for _, id := range ids {
			batch.ids[id] = struct{}{}
		}
		state := &s.runtimeState().frustrationFill
		state.mu.Lock()
		state.active = batch
		state.mu.Unlock()
		defer func() {
			state.mu.Lock()
			state.active = nil
			close(batch.done)
			state.mu.Unlock()
		}()
		counts, versions, err := s.scanFrustrationCounts(ctx, ids, column)
		if err != nil {
			return err
		}
		return s.storeFrustrationCounts(ctx, versions, counts, column)
	})
}

func (s *Store) canFillFrustrationCounts(ctx context.Context) (bool, error) {
	var writable bool
	err := s.pg.QueryRowContext(ctx, `SELECT NOT pg_is_in_recovery() AND has_table_privilege(to_regclass('session_frustration_counts'), 'SELECT') AND has_table_privilege(to_regclass('session_frustration_counts'), 'INSERT') AND has_table_privilege(to_regclass('session_frustration_counts'), 'UPDATE')`).Scan(&writable)
	return writable, err
}

// StartFrustrationFill prepares counts for writable replicas until stopped.
func (s *Store) StartFrustrationFill(ctx context.Context) func() {
	noop := func() {}
	if s.DB() == nil || s.readTx != nil || s.hostedRelations {
		return noop
	}
	writable, err := s.canFillFrustrationCounts(ctx)
	if err != nil || !writable {
		return noop
	}
	revision, _, err := s.LoadUsageChanges(ctx, math.MaxInt64)
	if err != nil || revision == 0 {
		return noop
	}
	oid, err := usageJournalOID(ctx, s.pg)
	if err != nil {
		return noop
	}
	column, err := sessionVersionColumnPG(ctx, s.pg)
	if err != nil {
		return noop
	}
	ctx, cancel := context.WithCancel(ctx)
	initial := true
	scheduler := poller.Start(ctx, poller.Job{Name: "frustration counts", Interval: 5 * time.Second, RunAtStart: true, Run: func(ctx context.Context) error {
		writable, err := s.canFillFrustrationCounts(ctx)
		if err != nil {
			if IsReadOnlyError(err) || isInsufficientPrivilege(err) {
				cancel()
			}
			return err
		}
		if !writable {
			cancel()
			return nil
		}
		currentOID, err := usageJournalOID(ctx, s.pg)
		if err != nil {
			if isUndefinedTable(err) || isInsufficientPrivilege(err) {
				cancel()
			}
			return err
		}
		current, changed, err := s.LoadUsageChanges(ctx, revision)
		if err != nil {
			return err
		}
		if current == 0 {
			cancel()
			return nil
		}
		newOID, err := usageJournalOID(ctx, s.pg)
		if err != nil {
			return err
		}
		if newOID != currentOID {
			initial = true
			return nil
		}
		if currentOID != oid || current < revision {
			initial = true
			revision = current
			oid = currentOID
		}
		if initial {
			ids, err := s.staleFrustrationSessions(ctx, column, nil)
			if err != nil {
				return err
			}
			if err := s.fillFrustrationCounts(ctx, ids, column); err != nil {
				if IsReadOnlyError(err) || isInsufficientPrivilege(err) {
					cancel()
				}
				return err
			}
			initial = false
			current, changed, err = s.LoadUsageChanges(ctx, revision)
			if err != nil {
				return err
			}
			if current == 0 {
				cancel()
				return nil
			}
			newOID, err := usageJournalOID(ctx, s.pg)
			if err != nil {
				return err
			}
			if newOID != oid || current < revision {
				initial = true
				return nil
			}
		}
		if len(changed) > 0 {
			ids, err := s.staleFrustrationSessions(ctx, column, changed)
			if err != nil {
				return err
			}
			if err := s.fillFrustrationCounts(ctx, ids, column); err != nil {
				if IsReadOnlyError(err) || isInsufficientPrivilege(err) {
					cancel()
				}
				return err
			}
		}
		revision = current
		return nil
	}})
	return func() { cancel(); scheduler.Wait() }
}
