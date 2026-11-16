package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"uuid"

	"go.kenn.io/agentsview/internal/db"
)

const standbyUsageMarker = "standby"

func (s *Store) directUsageVersion(ctx context.Context, slot string) string {
	if slot == "" || s.readTx != nil || s.hostedRelations {
		return ""
	}
	revision, err := s.directUsageChangeRevision(ctx)
	if err != nil || revision == 0 {
		return ""
	}
	runtime := s.runtimeState()
	runtime.pricingMu.Lock()
	defer runtime.pricingMu.Unlock()
	return fmt.Sprintf("%d|%d", revision, runtime.pricingGeneration)
}

func (s *Store) deferUsage(ctx context.Context, f db.UsageFilter) bool {
	if s.usageCache == nil || !s.usageCache.DeferRead(f) {
		return false
	}
	if s.runtimeState().usageIdentityDenied.Load() {
		return false
	}
	var standby bool
	return s.pg.QueryRowContext(ctx, `SELECT pg_is_in_recovery()`).Scan(&standby) == nil && !standby
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
	version := s.keptUsageDataVersion(ctx, slot)
	if version == "" || version == standbyUsageMarker {
		return version
	}
	runtime := s.runtimeState()
	runtime.pricingMu.Lock()
	defer runtime.pricingMu.Unlock()
	return fmt.Sprintf("%s|%d", version, runtime.pricingGeneration)
}

func (s *Store) keptUsageDataVersion(ctx context.Context, slot string) string {
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
	var systemID, timeline, historyToken string
	err := s.pg.QueryRowContext(ctx, `SELECT `+usageChangeRevisionSQL+`,
 to_regclass('usage_session_changes')::oid, (pg_control_system()).system_identifier::text,
 substring(pg_walfile_name(pg_current_wal_lsn()), 1, 8),
 COALESCE((SELECT value FROM sync_metadata WHERE key='usage_history_token'), '')
 WHERE NOT pg_is_in_recovery()`, strconv.Itoa(usageChangeJournalVersionPG)).Scan(&revision, &journalOID, &systemID, &timeline, &historyToken)
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
	if _, err := uuid.Parse(historyToken); err != nil {
		return ""
	}
	if s.usageCache != nil {
		after, err := s.usageCache.ResetEpoch(ctx)
		if err != nil || after != epoch {
			return ""
		}
	}
	return fmt.Sprintf("%d|%d|%s|%s|%d|%s", revision, journalOID.Int64, systemID, timeline, epoch, historyToken)
}

func (s *Store) keptDashboardVersion(ctx context.Context, panel string, f db.AnalyticsFilter) string {
	if s.readTx != nil || s.hostedRelations || s.runtimeState().usageIdentityDenied.Load() {
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
	var oid sql.NullInt64
	var systemID, timeline, common, token, scope, velocityJoin string
	err := s.pg.QueryRowContext(ctx, `SELECT to_regclass('usage_session_changes')::oid,
 (pg_control_system()).system_identifier::text, substring(pg_walfile_name(pg_current_wal_lsn()),1,8),
 COALESCE((SELECT value FROM sync_metadata WHERE key='dashboard_common'),''),
 COALESCE((SELECT value FROM sync_metadata WHERE key=$1),''),
 COALESCE((SELECT value FROM sync_metadata WHERE key='dashboard_scope'),''),
 COALESCE((SELECT value FROM sync_metadata WHERE key='dashboard_velocity_join'),'')
 WHERE NOT pg_is_in_recovery() AND EXISTS (SELECT 1 FROM sync_metadata WHERE key='usage_change_journal_version' AND value=$2)`, "dashboard_"+panel, strconv.Itoa(usageChangeJournalVersionPG)).Scan(&oid, &systemID, &timeline, &common, &token, &scope, &velocityJoin)
	if err != nil || !oid.Valid {
		return ""
	}
	for _, value := range []string{common, token} {
		if _, err := uuid.Parse(value); err != nil {
			return ""
		}
	}
	if db.DashboardUsesMessageScope(f) {
		if _, err := uuid.Parse(scope); err != nil {
			return ""
		}
	} else {
		scope = ""
	}
	if panel == "velocity" && f.Model != "" {
		if _, err := uuid.Parse(velocityJoin); err != nil {
			return ""
		}
	} else {
		velocityJoin = ""
	}
	if s.usageCache != nil {
		after, err := s.usageCache.ResetEpoch(ctx)
		if err != nil || after != epoch {
			return ""
		}
	}
	return fmt.Sprintf("%d|%s|%s|%d|%s|%s|%s|%s", oid.Int64, systemID, timeline, epoch, common, token, scope, velocityJoin)
}
