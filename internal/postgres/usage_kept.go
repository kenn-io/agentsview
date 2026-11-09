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
	runtime := s.runtimeState()
	runtime.pricingMu.Lock()
	defer runtime.pricingMu.Unlock()
	return fmt.Sprintf("%d|%d", revision, runtime.pricingGeneration)
}
