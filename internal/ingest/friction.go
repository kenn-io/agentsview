package ingest

import (
	"errors"
	"fmt"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/parser"
)

// FrictionOptions controls the rendering policy used when
// deriving a session's friction findings.
type FrictionOptions struct {
	RedactedToolRenderings bool
	Review                 func(friction.SessionInput) []friction.Signal
}

// ComputeSessionFriction derives the deterministic findings from the same
// normalized message rows that storage receives. Sessions over the review
// budget are marked skipped so backfill does not revisit them.
func ComputeSessionFriction(
	s db.Session, msgs []db.Message, pressureMax *float64, o FrictionOptions,
) (u db.SessionFrictionUpdate, err error) {
	if !db.FrictionInputFits(msgs) {
		return db.SkippedFriction(), nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("friction compute %s: panic: %v", s.ID, r)
		}
	}()

	dims := friction.Dims{Agent: s.Agent, Machine: s.Machine}
	raw, calls := FrictionRawInput(msgs)
	in := friction.BuildSessionInput(
		s.ID, dims, FrictionIsSubAgent(s), raw, calls,
		friction.BuildOptions{
			RedactedToolRenderings: o.RedactedToolRenderings,
			PressureMax:            pressureMax,
		},
	)
	review := o.Review
	if review == nil {
		review = friction.Review
	}
	sigs := review(in)

	u.RulesVersion = friction.RulesVersion
	u.Findings = make([]db.FrictionFinding, 0, len(sigs))
	for _, sig := range sigs {
		if sig.Kind == friction.KindError {
			sig.Text = friction.TruncateWithMarker(
				sig.Text, friction.MaxErrorMessageLength,
			)
		}
		title := sig.Title()
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
			Title:          title,
			Fingerprint:    friction.FingerprintTitle(title),
			Seq:            sig.Seq,
			RulesVersion:   friction.RulesVersion,
		}
		if !sig.OccurredAt.IsZero() {
			at := sig.OccurredAt.UTC()
			f.OccurredAt = &at
		}
		u.Findings = append(u.Findings, f)
	}
	u.Hash = db.FrictionHash(u.Findings, u.RulesVersion)
	return u, nil
}

// RefreshFriction replaces the derived friction state in a prepared session.
// Call it after provider-history reconciliation so the output describes the
// final projected rows.
func RefreshFriction(p *PreparedSession, options ContentOptions) error {
	if p == nil {
		return errors.New("refresh friction: nil prepared session")
	}
	if options.ArchiveContent.UsageOnly() {
		p.Friction = db.SettledFriction()
	} else {
		var err error
		p.Friction, err = ComputeSessionFriction(
			p.Session, p.Messages, p.Signals.ContextPressureMax,
			FrictionOptions{
				RedactedToolRenderings: options.ArchiveContent == config.ArchiveContentTranscripts,
			},
		)
		if err != nil {
			// Detectors are deterministic, so skip the session like the local
			// backfill does rather than fail the content write.
			log.Printf("friction: skipping %s: %v", p.Session.ID, err)
			p.Friction = db.SkippedFriction()
		}
	}
	p.Session.FrictionCount = len(p.Friction.Findings)
	p.Session.FrictionRulesVersion = p.Friction.RulesVersion
	p.Session.FrictionHash = p.Friction.Hash
	return nil
}

func FrictionRawInput(
	msgs []db.Message,
) ([]friction.RawMessage, []friction.RawToolCall) {
	raw := make([]friction.RawMessage, 0, len(msgs))
	var calls []friction.RawToolCall
	for _, m := range msgs {
		ts, _ := time.Parse(time.RFC3339Nano, m.Timestamp)
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
				FilePath:       tc.FilePath,
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

func FrictionIsSubAgent(s db.Session) bool {
	return s.ParentSessionID != nil && *s.ParentSessionID != "" &&
		s.RelationshipType == string(parser.RelSubagent)
}
