package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"go.kenn.io/agentsview/internal/db"
)

const standbyUsageMarker = "standby"

func (s *Store) directUsageVersion(ctx context.Context, slot string) string {
	if slot == "" || s.readTx != nil || s.hostedRelations {
		return ""
	}
	var revision int64
	err := s.pg.QueryRowContext(ctx, `SELECT coalesce((SELECT value::bigint FROM sync_metadata WHERE key = 'usage_change_revision' AND EXISTS (SELECT 1 FROM sync_metadata WHERE key = 'usage_change_journal_version' AND value IN ('1', '2'))), 0)`).Scan(&revision)
	if err != nil || revision == 0 {
		return ""
	}
	runtime := s.runtimeState()
	runtime.pricingMu.Lock()
	defer runtime.pricingMu.Unlock()
	return fmt.Sprintf("direct|%d|%d", revision, runtime.pricingGeneration)
}

func (s *Store) deferUsage(f db.UsageFilter) bool {
	return s.usageCache != nil && s.usageCache.DeferRead(f)
}

// standbyMarker returns standbyUsageMarker when the connected server is a
// standby, and "" otherwise.
func (s *Store) standbyMarker(ctx context.Context) string {
	var standby bool
	if s.pg.QueryRowContext(ctx, `SELECT pg_is_in_recovery()`).Scan(&standby) == nil && standby {
		return standbyUsageMarker
	}
	return ""
}

func (s *Store) keptUsageVersion(ctx context.Context, slot string) string {
	if slot == "" || s.readTx != nil || s.hostedRelations {
		return ""
	}
	var epoch int64
	if s.usageCache != nil {
		var err error
		epoch, err = s.usageCache.ResetEpoch(ctx)
		if err != nil {
			return s.standbyMarker(ctx)
		}
	}
	if s.runtimeState().usageIdentityDenied.Load() {
		return ""
	}
	var revision int64
	var journalOID sql.NullInt64
	var systemID, timeline string
	err := s.pg.QueryRowContext(ctx, `SELECT `+usageChangeRevisionSQL+`,
 to_regclass('usage_session_changes')::oid, (pg_control_system()).system_identifier::text,
 substring(pg_walfile_name(pg_current_wal_lsn()), 1, 8)
 WHERE NOT pg_is_in_recovery()`, strconv.Itoa(usageChangeJournalVersionPG)).Scan(&revision, &journalOID, &systemID, &timeline)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return standbyUsageMarker
		}
		if isInsufficientPrivilege(err) {
			s.runtimeState().usageIdentityDenied.Store(true)
		}
		return ""
	}
	s.runtimeState().usageIdentityDenied.Store(false)
	if revision == 0 || !journalOID.Valid {
		return ""
	}
	if s.usageCache != nil {
		after, err := s.usageCache.ResetEpoch(ctx)
		if err != nil || after != epoch {
			return ""
		}
	}
	runtime := s.runtimeState()
	runtime.pricingMu.Lock()
	defer runtime.pricingMu.Unlock()
	return fmt.Sprintf("%d|%d|%s|%s|%d|%d", revision, journalOID.Int64, systemID, timeline, epoch, runtime.pricingGeneration)
}
