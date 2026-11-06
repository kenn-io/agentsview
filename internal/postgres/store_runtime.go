package postgres

import (
	"context"
	"database/sql"
	"sync"

	"go.kenn.io/agentsview/internal/db"
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
			s.runtime = &storeRuntime{}
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

func (s *Store) supportsConcurrentReads() bool {
	return s.readTx == nil
}

// runIndependentReads runs reads that do not depend on each other. On the
// owned pool they run concurrently. A read snapshot shares one transaction,
// which cannot run statements at once, so there they run in order.
func (s *Store) runIndependentReads(reads ...func()) {
	if !s.supportsConcurrentReads() {
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
