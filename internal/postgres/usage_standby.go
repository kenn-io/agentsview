package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"strconv"

	"github.com/google/uuid"
	"go.kenn.io/agentsview/internal/kept"
)

// The journal token changes at every committed write, including across
// restored histories. It identifies direct reads on standbys and primaries
// whose roles cannot read the persistent cache's source identity.
func snapshotUsageVersion(ctx context.Context, q storeExecutor, generation uint64) string {
	var token string
	var revision int64
	var oid sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM sync_metadata WHERE key='usage_history_token'), ''), `+usageChangeRevisionSQL+`, to_regclass('usage_session_changes')::oid`, strconv.Itoa(usageChangeJournalVersionPG)).Scan(&token, &revision, &oid)
	if err != nil || revision == 0 || !oid.Valid {
		return ""
	}
	if _, err := uuid.Parse(token); err != nil {
		return ""
	}
	return fmt.Sprintf("snapshot|%s|%d|%d|%d", token, revision, oid.Int64, generation)
}

type snapshotResult[T any] struct {
	value   T
	handled bool
	version string
}

// A retained result is built entirely within the snapshot whose token
// identifies it. Failed or unavailable journal probes use the direct path.
func snapshotRetained[T any](ctx context.Context, s *Store, slot string, reads *kept.Reads[T], flights *kept.Flights[snapshotResult[T]], load func(context.Context, *Store) (T, error)) (T, bool, error) {
	var zero T
	if slot == "" || s.readTx != nil || s.hostedRelations {
		return zero, false, nil
	}
	runtime := s.runtimeState()
	runtime.pricingMu.Lock()
	generation := runtime.pricingGeneration
	runtime.pricingMu.Unlock()
	version := snapshotUsageVersion(ctx, s.pg, generation)
	if version == "" {
		return zero, false, nil
	}
	if value, ok := reads.Get(slot, version); ok {
		return value, true, nil
	}
	result, err := flights.Do(ctx, version+"|"+slot, func(ctx context.Context) (snapshotResult[T], error) {
		return snapshotLoad(ctx, s, slot, reads, load)
	})
	if err != nil {
		return zero, true, err
	}
	// A flight keyed by an old token can capture a newer snapshot. A caller
	// that observed the old token may have returned to that history while
	// the flight ran, so it must capture its own snapshot.
	if result.handled && result.version != version {
		result, err = snapshotLoad(ctx, s, slot, reads, load)
	}
	return result.value, result.handled, err
}

func snapshotLoad[T any](ctx context.Context, s *Store, slot string, reads *kept.Reads[T], load func(context.Context, *Store) (T, error)) (snapshotResult[T], error) {
	result := snapshotResult[T]{handled: true}
	tx, err := s.beginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	view := s.readView(tx)
	runtime := s.runtimeState()
	runtime.pricingMu.Lock()
	generation := runtime.pricingGeneration
	view.pricingSnapshot.custom = maps.Clone(runtime.customPricing)
	view.pricingSnapshot.frozen = true
	runtime.pricingMu.Unlock()
	result.version = snapshotUsageVersion(ctx, view.pg, generation)
	if result.version == "" {
		result.handled = false
		return result, nil
	}
	if value, ok := reads.Get(slot, result.version); ok {
		result.value = value
		return result, tx.Commit()
	}
	result.value, err = load(ctx, view)
	if err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	reads.Put(slot, result.version, result.value)
	return result, nil
}
