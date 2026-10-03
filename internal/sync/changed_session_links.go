package sync

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// changedSessionLinks collects only sessions reached by a changed-path batch.
type changedSessionLinks map[string]struct{}

// PendingSubagentLinks reports unfinished linking for a worker's terminal result.
func (e *Engine) PendingSubagentLinks() bool {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	return e.PendingSubagentLinksExclusive()
}

// PendingSubagentLinksExclusive snapshots retry state for a worker handoff.
// The caller holds syncMu.
func (e *Engine) PendingSubagentLinksExclusive() bool {
	return e.subagentLinkPending
}

// RetainSubagentLinkRetry adopts unfinished linking before reconciliation.
func (e *Engine) RetainSubagentLinkRetry(pending bool) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	e.RetainSubagentLinkRetryExclusive(pending)
}

// RetainSubagentLinkRetryExclusive retains a worker's unfinished linking without
// clearing any retry already owned by the daemon. The caller holds syncMu.
func (e *Engine) RetainSubagentLinkRetryExclusive(pending bool) {
	e.subagentLinkPending = e.subagentLinkPending || pending
}

// SetSubagentLinkRetryExclusive adopts the retry state after a worker completes
// archive-wide linking, including an audit that received the pending work.
// Unknown worker results must retain existing retries. The caller holds syncMu.
func (e *Engine) SetSubagentLinkRetryExclusive(pending bool) {
	e.subagentLinkPending = pending
}

func (ids changedSessionLinks) observe(job syncJob, prefix string) {
	if job.incremental != nil {
		ids[job.incremental.sessionID] = struct{}{}
	}
	for _, parsed := range job.results {
		ids[applyIDPrefixToID(prefix, parsed.Session.ID)] = struct{}{}
	}
	for _, id := range job.excludedSessionIDs {
		ids[applyIDPrefixToID(prefix, id)] = struct{}{}
	}
}

// link runs with syncMu held. A failure leaves the global link pending so an
// unchanged poll retries it even when the durable repair queue also failed.
func (ids changedSessionLinks) link(ctx context.Context, e *Engine, stats *SyncStats) error {
	if stats.Aborted {
		// Cancellation can leave committed writes whose new edges have not
		// been linked. Let the next poll finish without delaying cancellation.
		e.subagentLinkPending = e.subagentLinkPending || stats.Synced > 0
		return nil
	}
	if len(ids) == 0 {
		return nil
	}
	sessionIDs := make([]string, 0, len(ids))
	for id := range ids {
		sessionIDs = append(sessionIDs, id)
	}
	slices.Sort(sessionIDs)
	linked, err := e.linkSubagentSessionsForSessions(ctx, sessionIDs)
	if err != nil {
		e.subagentLinkPending = true
		linkErr := fmt.Errorf("link affected subagent sessions: %w", err)
		if queueErr := e.db.QueueSubagentParentRepairs(ctx, sessionIDs); queueErr != nil {
			return errors.Join(linkErr,
				fmt.Errorf("queue affected subagent parent repairs: %w", queueErr))
		}
		return linkErr
	}
	stats.RecordLinksUpdated(linked)
	return nil
}
