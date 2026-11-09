package postgres

import (
	"context"
	"fmt"
)

func (s *Store) keptUsageVersion(ctx context.Context, slot string) string {
	if slot == "" || s.readTx != nil || s.hostedRelations {
		return ""
	}
	revision, err := s.UsageChangeRevision(ctx)
	if err != nil || revision == 0 {
		return ""
	}
	var oid uint32
	if s.cachedUsage() != nil {
		var err error
		oid, err = usageJournalOID(ctx, s.pg)
		if err != nil {
			return ""
		}
	}
	runtime := s.runtimeState()
	runtime.pricingMu.Lock()
	defer runtime.pricingMu.Unlock()
	return fmt.Sprintf("%d|%d|%d", revision, runtime.pricingGeneration, oid)
}
