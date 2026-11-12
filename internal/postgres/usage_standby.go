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

// A standby cannot identify its timeline through pg_walfile_name. The journal
// token changes at every committed write, including across restored histories.
func standbyUsageVersion(ctx context.Context, q storeExecutor, generation uint64) string {
	var token string
	var revision int64
	var oid sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM sync_metadata WHERE key='usage_history_token'), ''), `+usageChangeRevisionSQL+`, to_regclass('usage_session_changes')::oid WHERE pg_is_in_recovery()`, strconv.Itoa(usageChangeJournalVersionPG)).Scan(&token, &revision, &oid)
	if err != nil || revision == 0 || !oid.Valid {
		return ""
	}
	if _, err := uuid.Parse(token); err != nil {
		return ""
	}
	return fmt.Sprintf("standby|%s|%d|%d|%d", token, revision, oid.Int64, generation)
}

// A retained standby result is built entirely within the snapshot whose token
// identifies it. Failed or unavailable journal probes use the direct path.
func standbyRetained[T any](ctx context.Context, s *Store, slot string, reads *kept.Reads[T], load func(context.Context, *Store) (T, error)) (T, bool, error) {
	var zero T
	if slot == "" || s.readTx != nil || s.hostedRelations {
		return zero, false, nil
	}
	runtime := s.runtimeState()
	runtime.pricingMu.Lock()
	generation := runtime.pricingGeneration
	runtime.pricingMu.Unlock()
	version := standbyUsageVersion(ctx, s.pg, generation)
	if version == "" {
		return zero, false, nil
	}
	if value, ok := reads.Get(slot, version); ok {
		return value, true, nil
	}
	tx, err := s.beginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return zero, true, err
	}
	defer func() { _ = tx.Rollback() }()
	view := s.readView(tx)
	runtime.pricingMu.Lock()
	generation = runtime.pricingGeneration
	view.pricingSnapshot.custom = maps.Clone(runtime.customPricing)
	view.pricingSnapshot.frozen = true
	runtime.pricingMu.Unlock()
	version = standbyUsageVersion(ctx, view.pg, generation)
	if version == "" {
		return zero, false, nil
	}
	if value, ok := reads.Get(slot, version); ok {
		return value, true, tx.Commit()
	}
	value, err := load(ctx, view)
	if err != nil {
		return zero, true, err
	}
	if err := tx.Commit(); err != nil {
		return zero, true, err
	}
	reads.Put(slot, version, value)
	return value, true, nil
}
