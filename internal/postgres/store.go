package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// Compile-time check: *Store satisfies db.Store.
var _ db.Store = (*Store)(nil)

// NewStore opens a PostgreSQL connection using the shared Open()
// helper and returns a Store.
// When allowInsecure is true, non-loopback connections without
// TLS produce a warning instead of failing.
func NewStore(
	pgURL, schema string, allowInsecure bool,
) (*Store, error) {
	pg, err := Open(pgURL, schema, allowInsecure)
	if err != nil {
		return nil, err
	}
	return newStore(pg), nil
}

// DB exposes the stable driver pool only to adapter-owned schema, connection,
// and vector capability setup. Server-facing application queries are owned by
// the embedded BunStore.
func (s *Store) DB() *sql.DB { return s.pg }

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.pg.Close()
}

// InsightGenerationAvailable reports whether startup proved this PG role can
// persist generated insights.
func (s *Store) InsightGenerationAvailable() bool {
	s.insightCapabilityMu.RLock()
	defer s.insightCapabilityMu.RUnlock()
	return s.insightGenerationAvailable
}

// InsightDeletionAvailable reports whether startup proved this PG role can
// delete persisted insights independently of generation privileges.
func (s *Store) InsightDeletionAvailable() bool {
	s.insightCapabilityMu.RLock()
	defer s.insightCapabilityMu.RUnlock()
	return s.insightDeletionAvailable
}

func (s *Store) setInsightCapabilities(insertAvailable, deleteAvailable bool) {
	s.insightCapabilityMu.Lock()
	defer s.insightCapabilityMu.Unlock()
	s.insightGenerationAvailable = insertAvailable
	s.insightDeletionAvailable = deleteAvailable
}

// DetectInsightGenerationAvailability probes whether this PG connection can
// insert and delete insights. PG serve uses generation availability to expose
// generate routes, while the shared store authorizes deletion independently.
func (s *Store) DetectInsightGenerationAvailability(
	ctx context.Context,
) error {
	insertAvailable, err := runInsightCapabilityProbe(
		ctx, s.pg, probeInsightGenerationAvailabilityTx,
	)
	if err != nil {
		return err
	}
	deleteAvailable, err := runInsightCapabilityProbe(
		ctx, s.pg, probeInsightDeletionAvailabilityTx,
	)
	if err != nil {
		return err
	}
	s.setInsightCapabilities(insertAvailable, deleteAvailable)
	return nil
}

func runInsightCapabilityProbe(
	ctx context.Context,
	pg *sql.DB,
	probe func(context.Context, *sql.Tx) (bool, error),
) (bool, error) {
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning insight capability probe: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return probe(ctx, tx)
}

func probeInsightGenerationAvailabilityTx(
	ctx context.Context, tx *sql.Tx,
) (bool, error) {
	var id int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO insights DEFAULT VALUES RETURNING id`,
	).Scan(&id); err != nil {
		if IsReadOnlyError(err) {
			return false, nil
		}
		return false, fmt.Errorf(
			"probing insight generation capability: %w", err,
		)
	}
	return true, nil
}

func probeInsightDeletionAvailabilityTx(
	ctx context.Context, tx *sql.Tx,
) (bool, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM insights WHERE FALSE`); err != nil {
		if IsReadOnlyError(err) {
			return false, nil
		}
		return false, fmt.Errorf("probing insight deletion capability: %w", err)
	}
	return true, nil
}

func mapPGWriteError(action string, err error) error {
	if err == nil {
		return nil
	}
	if IsReadOnlyError(err) {
		return fmt.Errorf("%s: %w: %w", action, db.ErrReadOnly, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}
