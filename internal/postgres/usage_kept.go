package postgres

import (
	"context"
	"fmt"
	"strconv"
)

func (s *Store) keptUsageVersion(ctx context.Context, slot string) string {
	if slot == "" || s.readTx != nil || s.hostedRelations {
		return ""
	}
	journal := s.keptUsageJournal(ctx)
	if journal == "" {
		return ""
	}
	runtime := s.runtimeState()
	runtime.pricingMu.Lock()
	defer runtime.pricingMu.Unlock()
	return fmt.Sprintf("%s|%d", journal, runtime.pricingGeneration)
}

// keptUsageJournal names the journal relation beside its revision, so a
// recreated journal that keeps its clock cannot validate memos built against
// the previous one. An unreadable revision disables memo reuse.
func (s *Store) keptUsageJournal(ctx context.Context) string {
	var revision int64
	var oid uint32
	err := s.pg.QueryRowContext(ctx, `SELECT `+usageChangeRevisionSQL+`, coalesce(to_regclass('usage_session_changes')::oid, 0)`,
		strconv.Itoa(usageChangeJournalVersionPG)).Scan(&revision, &oid)
	if err != nil || revision == 0 {
		return ""
	}
	return fmt.Sprintf("%d|%d", oid, revision)
}
