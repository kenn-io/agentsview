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
	"go.kenn.io/agentsview/internal/parser"
)

// FrictionDimsFunc supplies a session's friction dims (seat from
// user-configured seat patterns, NanoClaw persona/channel, and the trust
// filter's exclusion). PR 19 installs it; nil means no dims row and the
// coding correction detector. ok=false means "no dims for this session".
type FrictionDimsFunc func(ctx context.Context, s db.Session) (dims db.FrictionSessionDims, ok bool)

type frictionOptions struct {
	dims     FrictionDimsFunc
	redacted bool
	// review is a test seam; nil means friction.Review.
	review func(friction.SessionInput) []friction.Signal
}

func parseFrictionTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// frictionRawInput maps every stored message row to the friction adapter's
// raw rows; the adapter decides which rows the detectors see (spec §6.1,
// §6.8). CallIndex is the call's slice position, matching
// extractToolCallRows, so finding coordinates agree with signal and
// secret-finding coordinates. tool_calls carries no timestamp, so a call
// takes its message's.
func frictionRawInput(
	msgs []db.Message,
) ([]friction.RawMessage, []friction.RawToolCall) {
	raw := make([]friction.RawMessage, 0, len(msgs))
	var calls []friction.RawToolCall
	for _, m := range msgs {
		ts := parseFrictionTime(m.Timestamp)
		raw = append(raw, friction.RawMessage{
			Ordinal:           m.Ordinal,
			Role:              m.Role,
			Content:           m.Content,
			ThinkingText:      m.ThinkingText,
			IsSystem:          m.IsSystem,
			IsCompactBoundary: m.IsCompactBoundary,
			SourceSubtype:     m.SourceSubtype,
			Timestamp:         ts,
			ContextTokens:     m.ContextTokens,
			HasContextTokens:  m.HasContextTokens,
		})
		for ci, tc := range m.ToolCalls {
			c := friction.RawToolCall{
				MessageOrdinal: m.Ordinal,
				CallIndex:      ci,
				ToolName:       tc.ToolName,
				Category:       tc.Category,
				InputJSON:      tc.InputJSON,
				ResultContent:  tc.ResultContent,
				Timestamp:      ts,
			}
			if n := len(tc.ResultEvents); n > 0 {
				last := tc.ResultEvents[n-1]
				c.LastEventContent = last.Content
				c.EventStatus = last.Status
			}
			calls = append(calls, c)
		}
	}
	return raw, calls
}

// frictionIsSubAgent applies D13: a sub-agent has a parent session and the
// sub-agent relationship, not jilog's 16-zero id prefix.
func frictionIsSubAgent(s db.Session) bool {
	return s.ParentSessionID != nil && *s.ParentSessionID != "" &&
		s.RelationshipType == string(parser.RelSubagent)
}

func frictionDimsNonDefault(d db.FrictionSessionDims) bool {
	return d.Seat != "" || d.Persona != "" || d.Channel != "" ||
		d.DimsSource != "" || d.ReviewExcluded
}

// computeSessionFriction runs the friction detectors over a session's
// stored (projected) messages. pressureMax is the context pressure the
// signal pass computed for the same messages. A panic in the detectors is
// returned as an error so one bad session never aborts a sync.
func computeSessionFriction(
	ctx context.Context, s db.Session, msgs []db.Message,
	pressureMax *float64, o frictionOptions,
) (u db.SessionFrictionUpdate, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("friction compute %s: panic: %v", s.ID, r)
		}
	}()
	dims := friction.Dims{Agent: s.Agent, Machine: s.Machine}
	var row *db.FrictionSessionDims
	excluded := false
	if o.dims != nil {
		if d, ok := o.dims(ctx, s); ok {
			dims.Seat, dims.Persona, dims.Channel = d.Seat, d.Persona, d.Channel
			excluded = d.ReviewExcluded
			if frictionDimsNonDefault(d) {
				d.SessionID = s.ID
				row = &d
			}
		}
	}
	raw, calls := frictionRawInput(msgs)
	in := friction.BuildSessionInput(s.ID, dims, frictionIsSubAgent(s), raw, calls,
		friction.BuildOptions{
			RedactedToolRenderings: o.redacted,
			PressureMax:            pressureMax,
		})
	in.Excluded = excluded
	review := o.review
	if review == nil {
		review = friction.Review
	}
	sigs := review(in)

	u.RulesVersion = friction.RulesVersion
	u.Dims = row
	u.Findings = make([]db.FrictionFinding, 0, len(sigs))
	for _, sig := range sigs {
		f := db.FrictionFinding{
			SessionID:      s.ID,
			Kind:           string(sig.Kind),
			Detector:       sig.Detector,
			MessageOrdinal: sig.Ordinal,
			CallIndex:      sig.CallIndex,
			ToolName:       sig.ToolName,
			Label:          sig.Label,
			Text:           sig.Text,
			Evidence:       sig.Evidence,
			Title:          sig.Title(),
			Fingerprint:    sig.Fingerprint(),
			Seq:            sig.Seq,
			RulesVersion:   friction.RulesVersion,
		}
		if !sig.OccurredAt.IsZero() {
			at := sig.OccurredAt.UTC()
			f.OccurredAt = &at
		}
		u.Findings = append(u.Findings, f)
	}
	u.Hash = db.FrictionHash(u.Findings, u.Dims, u.RulesVersion)
	return u, nil
}

func (e *Engine) frictionOptions() frictionOptions {
	return frictionOptions{
		dims:     e.frictionDims,
		redacted: e.db.ArchiveContent() == config.ArchiveContentTranscripts,
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
// transcript revision still matches the rows it was computed from.
func (e *Engine) recomputeFrictionFromDB(ctx context.Context, sessionID string) error {
	sess, err := e.db.GetSessionFull(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("loading session %s: %w", sessionID, err)
	}
	if sess == nil {
		return nil
	}
	revision := ""
	if sess.TranscriptRevision != nil {
		revision = *sess.TranscriptRevision
	}
	msgs, err := e.db.GetAllMessages(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("loading messages %s: %w", sessionID, err)
	}
	fu, err := computeSessionFriction(
		ctx, *sess, msgs, sess.ContextPressureMax, e.frictionOptions(),
	)
	if err != nil {
		return err
	}
	applied, err := e.db.ReplaceSessionFrictionAtRevision(ctx, sessionID, revision, fu)
	if err != nil {
		return fmt.Errorf("publishing friction %s: %w", sessionID, err)
	}
	if !applied {
		return fmt.Errorf("session %s changed during friction recompute", sessionID)
	}
	return nil
}

// RecomputeFriction recomputes one session's friction from stored rows.
func (e *Engine) RecomputeFriction(ctx context.Context, sessionID string) error {
	if e.refuseWriteInForceParse("RecomputeFriction") {
		return errors.New("RecomputeFriction refused on report-only parse-diff engine")
	}
	return e.recomputeFrictionFromDB(ctx, sessionID)
}

const frictionBackfillPage = 200

// BackfillFriction computes findings for sessions with an older rules
// version. Each session takes the sync lock separately so live sync can
// interleave with a long backfill.
func (e *Engine) BackfillFriction(ctx context.Context) (int, error) {
	if e.refuseWriteInForceParse("BackfillFriction") {
		return 0, errors.New("BackfillFriction refused on report-only parse-diff engine")
	}
	if e.disableSignalRecompute {
		return 0, nil
	}
	ids, err := e.db.StaleFrictionSessions(ctx, friction.RulesVersion, 1)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		state, err := e.db.FrictionBackfillState(ctx)
		if err != nil {
			return 0, err
		}
		if state != "completed" {
			return 0, e.db.MarkFrictionBackfill(ctx, "completed", 0, 0, "")
		}
		return 0, nil
	}
	if err := e.db.MarkFrictionBackfill(ctx, "running", 0, 0, ""); err != nil {
		return 0, err
	}
	log.Printf("friction backfill: recomputing stale sessions")
	processed, failed := 0, 0
	var lastErr error
	cursor := ""
	for {
		ids, err := e.db.StaleFrictionSessionsAfter(
			ctx, friction.RulesVersion, cursor, frictionBackfillPage,
		)
		if err != nil {
			return processed, err
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return processed, err
			}
			cursor = id
			err := e.RunExclusive(func() error {
				return e.recomputeFrictionFromDB(ctx, id)
			})
			if err != nil {
				failed++
				lastErr = err
				log.Printf("friction backfill: %s: %v", id, err)
				continue
			}
			processed++
		}
		if err := e.db.MarkFrictionBackfill(
			ctx, "running", processed+failed, processed, "",
		); err != nil {
			return processed, err
		}
	}
	remaining, err := e.db.StaleFrictionSessions(ctx, friction.RulesVersion, 1)
	if err != nil {
		return processed, err
	}
	if failed > 0 {
		msg := fmt.Sprintf("%d sessions failed; last: %v", failed, lastErr)
		if err := e.db.MarkFrictionBackfill(
			ctx, "pending", processed+failed, processed, msg,
		); err != nil {
			return processed, err
		}
		return processed, fmt.Errorf("friction backfill incomplete: %s", msg)
	}
	if len(remaining) > 0 {
		// A concurrent writer introduced a stale session before the cursor.
		return processed, e.db.MarkFrictionBackfill(
			ctx, "pending", processed, processed, "",
		)
	}
	log.Printf("friction backfill: completed %d sessions", processed)
	return processed, e.db.MarkFrictionBackfill(
		ctx, "completed", processed, processed, "",
	)
}
