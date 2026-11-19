package main

import (
	"context"
	"errors"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/poller"
	"go.kenn.io/agentsview/internal/server"
)

const (
	conversationPruneJobName  = "conversation-tombstone-prune"
	conversationPruneInterval = time.Hour
	conversationPruneJitter   = 5 * time.Minute
)

// conversationPruneJob removes conversation export tombstones once they are
// past retention, so deleted and replaced messages stop holding disk space
// without an operator running db compact.
func conversationPruneJob(database *db.DB, idle *server.IdleTracker) poller.Job {
	return poller.Job{
		Name:       conversationPruneJobName,
		Interval:   conversationPruneInterval,
		Jitter:     conversationPruneJitter,
		RunAtStart: true,
		Run: func(ctx context.Context) error {
			done, ok := idle.BeginWork()
			if !ok {
				return nil
			}
			defer done()
			pruned, err := database.PruneExpiredConversationTombstones(ctx, time.Now(), db.ConversationTombstoneRetention)
			if errors.Is(err, db.ErrWriterClosed) {
				// Compaction or maintenance holds the writer; the next run retries.
				return nil
			}
			if pruned > 0 {
				log.Printf("conversation export: pruned %d expired tombstones", pruned)
			}
			return err
		},
	}
}
