package sync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
)

func (e *Engine) frictionOptionsForDB(store *db.DB) ingest.FrictionOptions {
	return ingest.FrictionOptions{
		RedactedToolRenderings: store.ArchiveContent() == config.ArchiveContentTranscripts,
		Review:                 e.frictionReviewHook,
	}
}

// attachFriction computes from the same projected messages as the signal
// pass. A detector failure leaves the update unset and the session stale for
// backfill without failing the content write.
func (e *Engine) attachFriction(
	u *db.SessionSignalUpdate, s db.Session, msgs []db.Message,
) {
	fu, err := ingest.ComputeSessionFriction(
		s, msgs, u.ContextPressureMax, e.frictionOptionsForDB(e.db),
	)
	if err != nil {
		log.Printf("friction: %v", err)
		return
	}
	u.Friction = &fu
}

// recomputeFrictionFromDatabase publishes a finding snapshot only when the
// transcript and hierarchy still match the inputs it was computed from.
func (e *Engine) recomputeFrictionFromDatabase(
	ctx context.Context, store *db.DB, sessionID string,
) (bool, error) {
	sess, err := store.GetSessionFull(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("loading session %s: %w", sessionID, err)
	}
	if sess == nil {
		return false, nil
	}
	fu := db.SettledFriction()
	// Usage-only and oversized sessions settle without loading transcript text.
	if !store.ArchiveContent().UsageOnly() {
		msgs, withinBudget, err := store.GetFrictionMessages(ctx, sessionID)
		if err != nil {
			return false, fmt.Errorf("loading messages %s: %w", sessionID, err)
		}
		fu = db.SkippedFriction()
		if withinBudget {
			projectedSession, msgs := store.ProjectSessionForStorage(*sess, msgs)
			fu, err = ingest.ComputeSessionFriction(
				projectedSession, msgs, projectedSession.ContextPressureMax,
				e.frictionOptionsForDB(store),
			)
			if err != nil {
				// Detectors are deterministic, so a retry would fail the same way.
				log.Printf("friction: skipping %s: %v", sessionID, err)
				fu = db.SkippedFriction()
			}
		}
	}
	applied, err := store.ReplaceSessionFrictionAtRevision(ctx, *sess, fu)
	if err != nil {
		return false, fmt.Errorf("publishing friction %s: %w", sessionID, err)
	}
	if !applied {
		return false, fmt.Errorf("session %s changed during friction recompute", sessionID)
	}
	return true, nil
}

const (
	frictionBackfillPage = 20
	// frictionBackfillBudget bounds one reconcile tick's review work. Each
	// session takes the sync lock separately, so sync waits at most one review.
	frictionBackfillBudget = 10 * time.Second
)

// BackfillFriction reviews stale sessions page by page until the tick budget
// runs out. Sync marks a session stale whenever its transcript or hierarchy
// changes, so this is also how growing sessions get fresh findings. Sessions
// that fail are passed over for the rest of the tick so they cannot starve
// later ones.
func (e *Engine) BackfillFriction(ctx context.Context) (int, error) {
	if e.refuseWriteInForceParse("BackfillFriction") {
		return 0, errors.New("BackfillFriction refused on report-only parse-diff engine")
	}
	e.frictionBackfillMu.Lock()
	defer e.frictionBackfillMu.Unlock()
	// Resync temporarily switches e.db and closes its connections. Protect the
	// page queries and progress writes as well as each session review.
	return e.backfillFriction(ctx, e.RunExclusive)
}

// DrainStaleFrictionLocked repeats backfill passes until one makes no
// progress, leaving only sessions that failed. Replica push work calls it
// inside SyncThenRun and its siblings, which already hold the sync lock.
func (e *Engine) DrainStaleFrictionLocked(ctx context.Context) error {
	held := func(work func() error) error { return work() }
	for {
		processed, err := e.backfillFriction(ctx, held)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			log.Printf("friction backfill: %v", err)
		}
		if processed == 0 {
			return nil
		}
	}
}

func (e *Engine) backfillFriction(
	ctx context.Context, exclusive func(func() error) error,
) (int, error) {
	var total int
	err := exclusive(func() error {
		// Usage-only archives settle empty findings without reading transcripts.
		if e.disableSignalRecompute && !e.db.ArchiveContent().UsageOnly() {
			return nil
		}
		var err error
		total, err = e.db.CountStaleFrictionSessions(ctx)
		if err != nil || total > 0 {
			return err
		}
		state, err := e.db.FrictionBackfillState(ctx)
		if err != nil || state == "completed" {
			return err
		}
		return e.db.MarkFrictionBackfill(ctx, "completed", 0, 0, "")
	})
	if err != nil || total == 0 {
		return 0, err
	}
	processed := 0
	var lastErr error
	failed := make(map[string]bool)
	deadline := time.Now().Add(frictionBackfillBudget)
	for time.Now().Before(deadline) {
		var ids []string
		err := exclusive(func() error {
			var err error
			ids, err = e.db.StaleFrictionSessions(ctx, frictionBackfillPage+len(failed))
			return err
		})
		if err != nil {
			return processed, err
		}
		ids = slices.DeleteFunc(ids, func(id string) bool { return failed[id] })
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if time.Now().After(deadline) {
				break
			}
			if err := ctx.Err(); err != nil {
				return processed, err
			}
			var applied bool
			err := exclusive(func() error {
				var err error
				applied, err = e.recomputeFrictionFromDatabase(ctx, e.db, id)
				return err
			})
			switch {
			case err != nil:
				failed[id], lastErr = true, err
				log.Printf("friction backfill: %s: %v", id, err)
			case applied:
				processed++
			}
		}
	}
	err = exclusive(func() error {
		remaining, err := e.db.CountStaleFrictionSessions(ctx)
		if err != nil {
			return err
		}
		state, detail := "pending", ""
		if remaining == 0 {
			state = "completed"
		}
		if lastErr != nil {
			detail = lastErr.Error()
		}
		return e.db.MarkFrictionBackfill(ctx, state, total, processed, detail)
	})
	if err != nil {
		return processed, err
	}
	return processed, lastErr
}
