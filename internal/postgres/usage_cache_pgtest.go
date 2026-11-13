//go:build pgtest

package postgres

import "context"

// WaitUsageCacheWarm waits for preparation queued by earlier reads.
func (s *Store) WaitUsageCacheWarm(ctx context.Context) error {
	if s.usageCache == nil {
		return nil
	}
	return s.usageCache.WaitWarm(ctx)
}
