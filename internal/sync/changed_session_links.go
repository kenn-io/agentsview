package sync

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// changedSessionLinks collects only sessions reached by a changed-path batch.
type changedSessionLinks map[string]struct{}

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
func (ids changedSessionLinks) link(ctx context.Context, e *Engine) error {
	if len(ids) == 0 {
		return nil
	}
	sessionIDs := make([]string, 0, len(ids))
	for id := range ids {
		sessionIDs = append(sessionIDs, id)
	}
	slices.Sort(sessionIDs)
	if err := e.db.LinkSubagentSessionsForSessions(ctx, sessionIDs); err != nil {
		e.subagentLinkPending = true
		linkErr := fmt.Errorf("link affected subagent sessions: %w", err)
		if queueErr := e.db.QueueSubagentParentRepairs(ctx, sessionIDs); queueErr != nil {
			return errors.Join(linkErr,
				fmt.Errorf("queue affected subagent parent repairs: %w", queueErr))
		}
		return linkErr
	}
	return nil
}
