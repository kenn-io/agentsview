package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

const (
	// walStallLogAttempts is how many consecutive periodic checkpoints must
	// leave pages behind before the stall is logged. One attempt overlapping
	// an ordinary read is not a stall.
	walStallLogAttempts = 3
	// readerPoolWaitLogThreshold is the time requests spent queued for a
	// reader connection within one interval before it is logged.
	readerPoolWaitLogThreshold = time.Second
)

// walCheckpointProgress is what a passive checkpoint reports: the pages in
// the WAL and how many of them have been copied into the database file.
type walCheckpointProgress struct {
	logPages          int
	checkpointedPages int
}

func (p walCheckpointProgress) unmovedPages() int {
	return max(0, p.logPages-p.checkpointedPages)
}

// checkpointWALPassive copies as many WAL pages into the database as open
// readers allow. It never waits for readers, so pages a reader still needs
// stay unmoved and show up in the returned progress.
func (db *DB) checkpointWALPassive(
	ctx context.Context,
) (walCheckpointProgress, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	var busy int
	var progress walCheckpointProgress
	err := db.getWriter().QueryRowContext(
		ctx, "PRAGMA wal_checkpoint(PASSIVE)",
	).Scan(&busy, &progress.logPages, &progress.checkpointedPages)
	if err != nil {
		return walCheckpointProgress{},
			fmt.Errorf("wal checkpoint passive: %w", err)
	}
	return progress, nil
}

// walStall describes a WAL that consecutive checkpoints could not drain.
type walStall struct {
	attempts     int
	logPages     int
	unmovedPages int
}

// walStallTracker counts consecutive checkpoints that left pages unmoved.
type walStallTracker struct {
	attempts int
}

// observe records one checkpoint result and reports a stall once
// walStallLogAttempts consecutive checkpoints have left pages unmoved.
func (t *walStallTracker) observe(
	progress walCheckpointProgress,
) (walStall, bool) {
	unmoved := progress.unmovedPages()
	if unmoved == 0 {
		t.attempts = 0
		return walStall{}, false
	}
	t.attempts++
	if t.attempts < walStallLogAttempts {
		return walStall{}, false
	}
	return walStall{
		attempts:     t.attempts,
		logPages:     progress.logPages,
		unmovedPages: unmoved,
	}, true
}

// readerPoolWaitDelta returns how many requests queued for a reader
// connection between two pool snapshots and how long they waited in total.
// A reopened pool restarts its counters, so a lower current count is measured
// from zero.
func readerPoolWaitDelta(
	previous, current sql.DBStats,
) (int64, time.Duration) {
	if current.WaitCount < previous.WaitCount ||
		current.WaitDuration < previous.WaitDuration {
		return current.WaitCount, current.WaitDuration
	}
	return current.WaitCount - previous.WaitCount,
		current.WaitDuration - previous.WaitDuration
}

// walDiagnostics is the periodic maintenance loop's state between ticks.
type walDiagnostics struct {
	stall       walStallTracker
	readerStats sql.DBStats
}

// walMaintenanceTick runs one periodic pass: it logs a WAL that is not
// draining and a reader pool that requests are queueing for, then truncates
// the WAL if it has grown past the threshold.
func (db *DB) walMaintenanceTick(ctx context.Context, diag *walDiagnostics) {
	progress, err := db.checkpointWALPassive(ctx)
	switch {
	case errors.Is(err, ErrWriterClosed):
		// A maintenance pass owns the writer; the next tick checks again.
	case err != nil:
		log.Printf("sqlite wal checkpoint: %v", err)
	default:
		if stall, ok := diag.stall.observe(progress); ok {
			log.Printf(
				"sqlite wal checkpoint: %d of %d pages unmoved after %d "+
					"consecutive attempts; a long-running reader is "+
					"holding the WAL",
				stall.unmovedPages, stall.logPages, stall.attempts,
			)
		}
	}

	if reader := db.rawReader(); reader != nil {
		current := reader.Stats()
		waits, waited := readerPoolWaitDelta(diag.readerStats, current)
		diag.readerStats = current
		if waited >= readerPoolWaitLogThreshold {
			log.Printf(
				"sqlite reader pool: %d requests waited %s in total for "+
					"one of %d connections over the last %s",
				waits, waited.Round(time.Millisecond),
				current.MaxOpenConnections, walCheckpointInterval,
			)
		}
	}

	attempted, err := db.MaybeCheckpointLargeWAL(ctx)
	if attempted && err != nil {
		log.Printf("sqlite wal checkpoint: %v", err)
	}
}
