package postgres

import (
	"context"
	"database/sql"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
)

// Keep captured revisions intact while making a proven stale prefix a member
// of its unique longest current copy. Retraction or divergence recomputes this
// choice from captures, never from the previously selected display payload.
func reconcileRawPrefixes(ctx context.Context, tx *sql.Tx, group string) error {
	branches, err := loadRawBranches(ctx, tx, "group_id", group)
	if err != nil {
		return err
	}
	overlays, err := loadRawOverlays(ctx, tx, group)
	if err != nil {
		return err
	}
	payloads := map[string]ingest.PreparedSession{}
	for _, b := range branches {
		if b.Active && !overlays.boolean(b.ID, "excluded") {
			payloads[b.CapturedSession] = ingest.PreparedSession{}
		}
	}
	if len(payloads) > 1 {
		for id := range payloads {
			payloads[id], err = loadRawPayload(ctx, tx, id)
			if err != nil {
				return err
			}
		}
	}
	// ponytail: compare the few device copies per group; cache prefix digests if
	// groups with many independent captures make this quadratic scan expensive.
	extends := map[string]map[string]bool{}
	for id, p := range payloads {
		extends[id] = map[string]bool{id: true}
		for other, q := range payloads {
			if id == other {
				continue
			}
			extends[id][other], err = rawTranscriptPrefix(p, q)
			if err != nil {
				return err
			}
		}
	}
	for _, b := range branches {
		if !b.Active || overlays.boolean(b.ID, "excluded") {
			continue
		}
		selected := b.CapturedSession
		for id := range payloads {
			if extends[selected][id] {
				selected = id
			}
		}
		// A common prefix of divergent continuations supplies no evidence for
		// selecting either one, so retain it as a separate variant.
		for id := range payloads {
			if extends[b.CapturedSession][id] && !extends[id][selected] {
				selected = b.CapturedSession
				break
			}
		}
		if b.Session != selected {
			_, err = tx.ExecContext(ctx, `UPDATE raw_session_branches b SET session_id=c.session_id,content_revision=c.content_revision FROM raw_content_revisions c WHERE b.branch_id=$1 AND c.session_id=$2`, b.ID, selected)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Prefix equality includes complete normalized messages and usage events, not
// just rendered text. Metadata disagreements remain conflicts; only aggregate
// and signal fields that naturally change on append are excluded.
func rawTranscriptPrefix(short, long ingest.PreparedSession) (bool, error) {
	if len(short.Messages) == 0 || len(short.Messages) >= len(long.Messages) || len(short.UsageEvents) > len(long.UsageEvents) {
		return false, nil
	}
	long.Messages = long.Messages[:len(short.Messages)]
	long.UsageEvents = long.UsageEvents[:len(short.UsageEvents)]
	key := func(p ingest.PreparedSession) (string, error) {
		s := &p.Session
		s.EndedAt, s.TerminationStatus = nil, nil
		s.MessageCount, s.UserMessageCount = 0, 0
		s.TotalOutputTokens, s.PeakContextTokens = 0, 0
		s.HasTotalOutputTokens, s.HasPeakContextTokens, s.IsAutomated = false, false, false
		copyRawSignalFields(s, db.SessionSignalUpdate{})
		s.QualitySignals = nil
		p.Signals = db.SessionSignalUpdate{}
		p.Findings = nil
		return rawContentRevision(p)
	}
	a, err := key(short)
	if err != nil {
		return false, err
	}
	b, err := key(long)
	return a == b, err
}
