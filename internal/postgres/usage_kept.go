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
			return s.standbyUsageVersion(ctx)
		}
	}
	var revision int64
	var journalOID sql.NullInt64
	var systemID, timeline string
	err := s.pg.QueryRowContext(ctx, `SELECT `+usageChangeRevisionSQL+`,
 to_regclass('usage_session_changes')::oid, (pg_control_system()).system_identifier::text,
 substring(pg_walfile_name(pg_current_wal_lsn()), 1, 8)
 WHERE NOT pg_is_in_recovery()`, strconv.Itoa(usageChangeJournalVersionPG)).Scan(&revision, &journalOID, &systemID, &timeline)
	if err != nil || revision == 0 || !journalOID.Valid {
		return s.standbyUsageVersion(ctx)
	}
	if s.usageCache != nil {
		after, err := s.usageCache.ResetEpoch(ctx)
		if err != nil || after != epoch {
			return s.standbyUsageVersion(ctx)
		}
	}
	runtime := s.runtimeState()
	runtime.pricingMu.Lock()
	defer runtime.pricingMu.Unlock()
	return fmt.Sprintf("%d|%d|%s|%s|%d|%d", revision, journalOID.Int64, systemID, timeline, epoch, runtime.pricingGeneration)
}

// A standby serves direct reads, so its retained results use the journal and
// pricing generation. The prefix prevents reuse across a change of server role.
func (s *Store) standbyUsageVersion(ctx context.Context) string {
	var revision int64
	err := s.pg.QueryRowContext(ctx, `SELECT `+usageChangeRevisionSQL+` WHERE pg_is_in_recovery()`, strconv.Itoa(usageChangeJournalVersionPG)).Scan(&revision)
	if err != nil || revision == 0 {
		return ""
	}
	runtime := s.runtimeState()
	runtime.pricingMu.Lock()
	defer runtime.pricingMu.Unlock()
	return fmt.Sprintf("standby|%d|%d", revision, runtime.pricingGeneration)
}
