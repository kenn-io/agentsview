package postgres

import (
	"context"
	"database/sql"
	"slices"
	"sync"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/kept"
)

// storeExecutor keeps every query on either the owned pool or one read snapshot.
// Pool ownership and transaction creation are deliberately separate.
type storeExecutor interface {
	hostedQuerier
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Store) runtimeState() *storeRuntime {
	s.runtimeOnce.Do(func() {
		if s.runtime == nil {
			s.runtime = &storeRuntime{
				activityKept: kept.Memo[pgActivityKept]{
					MaxBytes: 128 << 20,
					Size: func(e pgActivityKept) int64 {
						size := activity.EstimatedArtifactBytes(e.report.Artifacts)
						for _, id := range slices.Concat(e.candidates, e.peers, e.labels) {
							size += int64(len(id)) + 16
						}
						return size
					},
				},
			}
		}
	})
	return s.runtime
}

func (s *Store) requireOwnedPool() (*sql.DB, error) {
	pool := s.DB()
	if pool == nil {
		return nil, db.ErrReadOnly
	}
	return pool, nil
}

func (s *Store) beginTx(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
	pool, err := s.requireOwnedPool()
	if err != nil {
		return nil, err
	}
	return pool.BeginTx(ctx, options)
}

func (s *Store) readView(tx *sql.Tx) *Store {
	return &Store{
		pg: tx, readTx: tx, hostedRelations: s.hostedRelations,
		runtime: s.runtimeState(), pricingSnapshot: &pricingSnapshot{},
	}
}

// runIndependentReads runs reads that do not depend on each other. On the
// owned pool they run concurrently. A read snapshot shares one transaction,
// which cannot run statements at once, so there they run in order.
func (s *Store) runIndependentReads(reads ...func()) {
	if s.readTx != nil {
		for _, read := range reads {
			read()
		}
		return
	}
	var wg sync.WaitGroup
	for _, read := range reads {
		wg.Go(read)
	}
	wg.Wait()
}
