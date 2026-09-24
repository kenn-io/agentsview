package review

import (
	"context"
	"log"

	"github.com/google/uuid"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/frictionevents"
	"go.kenn.io/agentsview/internal/ledger"
)

// patternsBefore reads friction_patterns for this build's fingerprints
// before the digest transaction. It returns nil (skip pattern events) when
// the ledger is off or the read fails.
func (r *Runner) patternsBefore(ctx context.Context, sigs []friction.Signal) map[string]db.FrictionPattern {
	if r.Ledger == nil {
		return nil
	}
	seen := map[string]bool{}
	var fps []string
	for _, s := range sigs {
		if fp := s.Fingerprint(); !seen[fp] {
			seen[fp] = true
			fps = append(fps, fp)
		}
	}
	before, err := r.Store.FrictionPatternsByFingerprint(ctx, fps)
	if err != nil {
		log.Printf("friction review: pattern events skipped: %v", err)
		return nil
	}
	return before
}

func patternTransitions(
	sigs []friction.Signal, before map[string]db.FrictionPattern, date string, runID *uuid.UUID,
) (first, recurred []frictionevents.PatternTransition) {
	counts := map[string]int{}
	var order []friction.Signal
	for _, s := range sigs {
		fp := s.Fingerprint()
		if counts[fp] == 0 {
			order = append(order, s)
		}
		counts[fp]++
	}
	for _, s := range order {
		fp := s.Fingerprint()
		t := frictionevents.PatternTransition{
			Fingerprint: fp, Title: s.Title(), SubjectID: s.SubjectID,
			Date: date, SignalKind: s.Kind, RunID: runID,
		}
		prior, ok := before[fp]
		if !ok || prior.FirstSeenDate == date {
			first = append(first, t)
			continue
		}
		t.OccurrenceCount = prior.OccurrenceCount + counts[fp]
		recurred = append(recurred, t)
	}
	return first, recurred
}

// emitBuildEvents appends §12.5's build events after the digest commit.
// Failures are logged; the digest stands and the next build does not replay
// them (§20).
func (r *Runner) emitBuildEvents(
	ctx context.Context, date string, rep Report, digest db.FrictionDigest, before map[string]db.FrictionPattern,
) {
	if r.Ledger == nil {
		return
	}
	now := r.now()
	runID := frictionevents.ParseRunID(digest.RunID)
	events := []ledger.Event{frictionevents.DigestBuiltEvent(r.LedgerSource,
		frictionevents.DigestBuiltFromSnapshot(rep.Snapshot, rep.Meta, digest.Revision, digest.MarkdownSHA256, runID), now)}
	if before != nil {
		first, recurred := patternTransitions(rep.Snapshot.Signals, before, date, runID)
		for _, p := range first {
			events = append(events, frictionevents.PatternFirstSeenEvent(r.LedgerSource, p, now))
		}
		for _, p := range recurred {
			events = append(events, frictionevents.PatternRecurredEvent(r.LedgerSource, p, now))
		}
	}
	if err := r.Ledger.Append(ctx, "", events); err != nil {
		log.Printf("friction review %s: ledger append failed (digest kept): %v", date, err)
	}
}
