package main

import (
	"context"
	"errors"
	"log"
	"time"

	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func syncLifecycleOutcome(ctx context.Context, err error) string {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return "canceled"
	}
	if err != nil {
		return "failed"
	}
	return "completed"
}

// completeWorkerStartupReconciliation closes the worker-to-watcher event gap
// and acknowledges the worker result. The callbacks keep this orchestration
// testable while preserving the engine's existing lock and dispatch behavior.
func completeWorkerStartupReconciliation(
	ctx context.Context,
	roots []string,
	workerStats syncpkg.SyncStats,
	reconcile func(context.Context, []string, bool) error,
	queueRetry func(syncpkg.WatchBatch),
	record func(syncpkg.SyncStats, error),
) {
	started := time.Now()
	log.Printf("startup gap reconciliation started: roots=%d", len(roots))

	var gapErr error
	if len(roots) > 0 {
		gapErr = reconcile(ctx, roots, false)
	}
	if gapErr != nil && ctx.Err() == nil {
		// Hand the failed gap reconciliation to the watcher's retry queue before
		// dispatch opens, as the original inline startup path did.
		queueRetry(gapReconciliationRetryBatch(gapErr))
	}
	record(workerStats, gapErr)

	outcome := syncLifecycleOutcome(ctx, gapErr)
	if gapErr != nil {
		log.Printf(
			"startup gap reconciliation finished: roots=%d duration=%s outcome=%s error_type=%T",
			len(roots), time.Since(started).Round(time.Millisecond), outcome, gapErr,
		)
		return
	}
	log.Printf(
		"startup gap reconciliation finished: roots=%d duration=%s outcome=%s",
		len(roots), time.Since(started).Round(time.Millisecond), outcome,
	)
}
