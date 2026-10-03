package sync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/ingest"
)

type frictionOptions struct {
	redacted bool
	// review is a test seam; nil means friction.Review.
	review func(friction.SessionInput) []friction.Signal
}

// computeSessionFriction runs the friction detectors over a session's
// stored (projected) messages. pressureMax is the context pressure the
// signal pass computed for the same messages. A panic in the detectors is
// returned as an error so one bad session never aborts a sync.
func computeSessionFriction(
	ctx context.Context, s db.Session, msgs []db.Message,
	pressureMax *float64, o frictionOptions,
) (u db.SessionFrictionUpdate, err error) {
	return ingest.ComputeSessionFriction(s, msgs, pressureMax,
		ingest.FrictionOptions{
			RedactedToolRenderings: o.redacted,
			Review:                 o.review,
		})
}

func (e *Engine) frictionOptions() frictionOptions {
	return e.frictionOptionsForDB(e.db)
}

func (e *Engine) frictionOptionsForDB(store *db.DB) frictionOptions {
	return frictionOptions{
		redacted: store.ArchiveContent() == config.ArchiveContentTranscripts,
		review:   e.frictionReviewHook,
	}
}

// attachFriction computes from the same projected messages as the signal
// pass. A detector failure leaves the update unset and the session stale for
// backfill without failing the content write.
func (e *Engine) attachFriction(
	u *db.SessionSignalUpdate, s db.Session, msgs []db.Message,
) {
	fu, err := computeSessionFriction(
		context.Background(), s, msgs, u.ContextPressureMax, e.frictionOptions(),
	)
	if err != nil {
		log.Printf("friction: %v", err)
		return
	}
	u.Friction = &fu
}

// recomputeFrictionFromDB publishes a finding snapshot only when the
// transcript and hierarchy still match the inputs it was computed from.
func (e *Engine) recomputeFrictionFromDB(ctx context.Context, sessionID string) error {
	_, err := e.recomputeFrictionFromDatabase(ctx, e.db, sessionID)
	return err
}

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
	// Usage-only settlement needs no transcript input, even for large archives.
	if store.ArchiveContent().UsageOnly() {
		return store.ReplaceSessionFrictionAtRevision(ctx, *sess, db.SessionFrictionUpdate{})
	}
	msgs, withinBudget, err := store.GetFrictionMessages(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("loading messages %s: %w", sessionID, err)
	}
	if !withinBudget {
		return false, nil
	}
	projectedSession, msgs := store.ProjectSessionForStorage(*sess, msgs)
	options := e.frictionOptionsForDB(store)
	fu, err := computeSessionFriction(
		ctx, projectedSession, msgs, projectedSession.ContextPressureMax,
		options,
	)
	if err != nil {
		return false, err
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

const frictionBackfillPage = 20

// BackfillFriction reviews at most one small page per reconcile tick. The
// saved cursor advances past failures and oversized sessions so they cannot
// starve later sessions after a restart. Each review takes the sync lock
// separately.
func (e *Engine) BackfillFriction(ctx context.Context) (int, error) {
	const cursorKey = "friction_backfill_cursor"
	if e.refuseWriteInForceParse("BackfillFriction") {
		return 0, errors.New("BackfillFriction refused on report-only parse-diff engine")
	}
	e.frictionBackfillMu.Lock()
	defer e.frictionBackfillMu.Unlock()
	// Resync temporarily switches e.db and closes its connections. Protect the
	// page queries and progress writes as well as each session review.
	var total int
	var ids []string
	var cursor string
	err := e.RunExclusive(func() error {
		// Usage-only archives settle empty findings without reading transcripts.
		if e.disableSignalRecompute && !e.db.ArchiveContent().UsageOnly() {
			return nil
		}
		var err error
		total, err = e.db.CountStaleFrictionSessions(ctx, friction.RulesVersion)
		if err != nil {
			return err
		}
		if total == 0 {
			state, err := e.db.FrictionBackfillState(ctx)
			if err != nil {
				return err
			}
			if state == "completed" {
				return nil
			}
			return e.db.MarkFrictionBackfill(ctx, "completed", 0, 0, "")
		}
		cursor, err = e.db.GetSyncState(ctx, cursorKey)
		if err != nil {
			return fmt.Errorf("loading friction backfill cursor: %w", err)
		}
		ids, err = e.db.StaleFrictionSessionsAfter(ctx, friction.RulesVersion, cursor, frictionBackfillPage)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			cursor = ""
			ids, err = e.db.StaleFrictionSessions(ctx, friction.RulesVersion, frictionBackfillPage)
		}
		return err
	})
	if err != nil || total == 0 {
		return 0, err
	}
	processed := 0
	var lastErr error
	deadline := time.Now().Add(2 * time.Second)
	for _, id := range ids {
		if time.Now().After(deadline) {
			break
		}
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		cursor = id
		var applied bool
		err := e.RunExclusive(func() error {
			var err error
			applied, err = e.recomputeFrictionFromDatabase(ctx, e.db, id)
			return err
		})
		if err != nil {
			lastErr = err
			log.Printf("friction backfill: %s: %v", id, err)
		} else if applied {
			processed++
		}
	}
	err = e.RunExclusive(func() error {
		remaining, err := e.db.CountStaleFrictionSessions(ctx, friction.RulesVersion)
		if err != nil {
			return err
		}
		state, detail := "pending", ""
		if remaining == 0 {
			state = "completed"
			cursor = ""
		}
		if lastErr != nil {
			detail = lastErr.Error()
		}
		if err := e.db.SetSyncState(ctx, cursorKey, cursor); err != nil {
			return fmt.Errorf("saving friction backfill cursor: %w", err)
		}
		return e.db.MarkFrictionBackfill(ctx, state, total, processed, detail)
	})
	if err != nil {
		return processed, err
	}
	return processed, lastErr
}
