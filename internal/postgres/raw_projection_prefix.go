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
	if len(short.Messages) == 0 || len(short.UsageEvents) > len(long.UsageEvents) {
		return false, nil
	}
	// A capture with the same transcript extends another only when the agent's
	// cumulative session total grew, as when Claude Code appends cost-state on
	// exit without a new message. A strict order keeps two equal copies from
	// extending each other.
	if len(short.Messages) > len(long.Messages) ||
		(len(short.Messages) == len(long.Messages) &&
			(len(short.UsageEvents) != len(long.UsageEvents) ||
				!rawSessionTotalGrew(short.SessionTotal, long.SessionTotal))) {
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
		// Plan usage and the agent's session total are later reports about
		// the whole thread, so they change as the thread grows.
		p.PlanUsage = nil
		p.SessionTotal = nil
		return rawContentRevision(p)
	}
	a, err := key(short)
	if err != nil {
		return false, err
	}
	b, err := key(long)
	return a == b, err
}

// rawSessionTotalGrew reports whether next is a later cumulative snapshot of
// the agent's session total than prev: it records more cost, or the same cost
// over more tokens.
func rawSessionTotalGrew(prev, next *db.SessionUsageTotal) bool {
	if next == nil {
		return false
	}
	if prev == nil {
		return true
	}
	if prevCost, nextCost := rawSessionTotalCost(prev), rawSessionTotalCost(next); nextCost != prevCost {
		return nextCost > prevCost
	}
	return rawSessionTotalTokens(next) > rawSessionTotalTokens(prev)
}

func rawSessionTotalCost(t *db.SessionUsageTotal) int64 {
	if t.Cost == nil {
		return -1
	}
	return t.Cost.Microdollars
}

func rawSessionTotalTokens(t *db.SessionUsageTotal) int64 {
	var total int64
	for _, model := range t.Models {
		total += model.InputTokens + model.OutputTokens +
			model.CacheCreationInputTokens + model.CacheReadInputTokens
	}
	return total
}
