package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

func (s *Store) keptUsageVersion(ctx context.Context, slot string) string {
	if slot == "" || s.readTx != nil || s.hostedRelations {
		return ""
	}
	var epoch int64
	if s.usageCache != nil {
		var err error
		epoch, err = s.usageCache.ResetEpoch(ctx)
		if err != nil {
			return ""
		}
	}
	var revision int64
	var journalOID sql.NullInt64
	var systemID, timeline string
	err := s.pg.QueryRowContext(ctx, `SELECT `+usageChangeRevisionSQL+`,
 to_regclass('usage_session_changes')::oid, (pg_control_system()).system_identifier::text,
 substring(pg_walfile_name(pg_current_wal_lsn()), 1, 8)
 WHERE NOT pg_is_in_recovery()`, strconv.Itoa(usageChangeJournalVersionPG)).Scan(&revision, &journalOID, &systemID, &timeline)
	if err != nil {
		return ""
	}
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
