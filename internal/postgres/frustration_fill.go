package postgres

import (
	"context"
	"errors"
	"log"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// Bound a slow chunk so later chunks can still be prepared.
var frustrationFillChunkTimeout = 30 * time.Second

func (s *Store) fillFrustrationCounts(ctx context.Context, ids []string, column string) error {
	var firstErr error
	for chunk := range slices.Chunk(ids, maxPGVars) {
		chunkCtx, cancel := context.WithTimeout(ctx, frustrationFillChunkTimeout)
		counts, versions, err := s.scanFrustrationCounts(chunkCtx, chunk, column)
		if err == nil {
			err = s.storeFrustrationCountsWithLockTimeout(chunkCtx, versions, counts, column)
		}
		cancel()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// StartFrustrationFill prepares stale counts after the launch Usage warm-up.
func (s *Store) StartFrustrationFill(ctx context.Context) func() {
	warmErr := s.WarmUsageCache(ctx)
	if warmErr != nil && !errors.Is(warmErr, db.ErrUsageSourceUnavailable) {
		log.Printf("pg serve: queueing Usage cache warm: %v", warmErr)
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if warmErr == nil && s.usageCache != nil {
			if err := s.usageCache.WaitWarmAttempt(ctx); err != nil {
				return
			}
		}
		writable, err := s.canFillFrustrationCounts(ctx)
		if err != nil {
			log.Printf("checking frustration count fill: %v", err)
			return
		}
		if !writable {
			return
		}
		column, err := sessionVersionColumnPG(ctx, s.pg)
		if err != nil {
			log.Printf("selecting frustration count version: %v", err)
			return
		}
		ids, err := s.staleFrustrationSessions(ctx, column)
		if err != nil {
			log.Printf("selecting stale frustration counts: %v", err)
			return
		}
		if err := s.fillFrustrationCounts(ctx, ids, column); err != nil && ctx.Err() == nil {
			log.Printf("filling frustration counts: %v", err)
		}
	}()
	return func() { cancel(); <-done }
}
