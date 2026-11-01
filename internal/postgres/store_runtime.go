package postgres

import (
	"context"
	"database/sql"

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

func (s *Store) beginTx(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
	pool := s.DB()
	if pool == nil {
		return nil, db.ErrReadOnly
	}
	return pool.BeginTx(ctx, options)
}

func (s *Store) readView(tx *sql.Tx) *Store {
	return &Store{
		pg: tx, readTx: tx, hostedRelations: s.hostedRelations,
		runtime: s.runtimeState(), pricingSnapshot: &pricingSnapshot{},
	}
}
