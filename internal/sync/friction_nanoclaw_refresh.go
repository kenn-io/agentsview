package sync

import (
	"context"
	"errors"
	"fmt"
	"log"

	"go.kenn.io/agentsview/internal/nanoclaw"
)

// NanoClawFingerprintKey records in pg_sync_state the resolver fingerprint
// the stored NanoClaw dims were computed with.
const NanoClawFingerprintKey = "friction_nanoclaw_fingerprint"

// RefreshNanoClawFriction recomputes friction for every archived cell
// session when the NanoClaw agent map or its configuration changed since
// the last refresh, and returns how many sessions it recomputed. A failed
// recompute leaves the old fingerprint in place so the next tick retries.
func (e *Engine) RefreshNanoClawFriction(ctx context.Context, resolver *nanoclaw.Resolver) (int, error) {
	if resolver == nil {
		return 0, nil
	}
	fingerprint := resolver.Fingerprint(ctx)
	stored, err := e.db.GetSyncState(ctx, NanoClawFingerprintKey)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", NanoClawFingerprintKey, err)
	}
	if stored == fingerprint {
		return 0, nil
	}
	seen := map[string]bool{}
	recomputed, failed := 0, false
	for _, root := range resolver.SessionRoots() {
		ids, err := e.db.SessionIDsUnderPath(ctx, root)
		if err != nil {
			return recomputed, err
		}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			if err := e.RecomputeFriction(ctx, id); err != nil {
				log.Printf("nanoclaw friction refresh: %s: %v", id, err)
				failed = true
				continue
			}
			recomputed++
		}
	}
	if failed {
		return recomputed, errors.New("nanoclaw friction refresh: some sessions failed; will retry")
	}
	if err := e.db.SetSyncState(ctx, NanoClawFingerprintKey, fingerprint); err != nil {
		return recomputed, fmt.Errorf("writing %s: %w", NanoClawFingerprintKey, err)
	}
	return recomputed, nil
}
