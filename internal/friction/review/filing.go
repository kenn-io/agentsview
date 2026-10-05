package review

import (
	"context"
	"fmt"
	"log"

	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
)

// Filer is the Kata filer seen by the runner (spec §26.5). It is set only on
// the agentsview filing hub with [kata] enabled (§13.0); everywhere else
// Runner.Filer is nil and step 8 is skipped.
type Filer interface {
	Ready(ctx context.Context) bool
	SnapshotOpen(ctx context.Context, fingerprints []string) (map[string]bool, error)
	FileAll(ctx context.Context, sigs []friction.Signal, run filing.RunContext) (filing.Report, error)
	Links(ctx context.Context, fingerprints []string) (map[string]friction.IssueRef, error)
	Drain(ctx context.Context) (filing.Report, error)
}

// annotatable reports whether a kind takes digest issue annotations
// (deferrals and interruptions never do; spec §8.3 step 8, §6.8).
func annotatable(k friction.Kind) bool {
	return k != friction.KindDeferral && k != friction.KindInterruption
}

func annotatableFingerprints(sigs []friction.Signal) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sigs {
		if !annotatable(s.Kind) {
			continue
		}
		if fp := s.Fingerprint(); !seen[fp] {
			seen[fp] = true
			out = append(out, fp)
		}
	}
	return out
}

func (r *Runner) filingRun(date string) filing.RunContext {
	return filing.RunContext{Date: date, PublicURL: r.PublicURL, DigestURL: filing.DigestURL(r.PublicURL, date)}
}

// fileAndLink is spec §8.3 step 8 for a non-dry-run build. With Kata not
// ready it only queues pending outbox rows (auto_file) and renders no links.
// Kata problems are logged and never fail the build (§20).
func (r *Runner) fileAndLink(ctx context.Context, date string, snap *friction.DigestSnapshot, meta *friction.SummaryMeta,
	usage map[string]friction.SessionUsage,
) (friction.RenderLinks, error) {
	if r.Filer == nil {
		return friction.RenderLinks{}, nil
	}
	if !r.Filer.Ready(ctx) {
		if r.AutoFile {
			if _, err := r.Filer.FileAll(ctx, snap.Signals, r.filingRun(date)); err != nil {
				return friction.RenderLinks{}, fmt.Errorf("queueing Kata filings: %w", err)
			}
		}
		return r.linksFor(ctx, date, snap.Signals)
	}
	// Snapshot already-open linked issues before filing. A new issue from this
	// build cannot count as a recurrence in the same digest.
	open, err := r.Filer.SnapshotOpen(ctx, annotatableFingerprints(snap.Signals))
	if err != nil {
		log.Printf("friction review: %s: open-issue snapshot failed (preserving prior recurrence annotations): %v", date, err)
	} else {
		costs := map[string]friction.USD{}
		for id, u := range usage {
			if u.CostUSD != nil {
				costs[id] = *u.CostUSD
			}
		}
		snap.RecurrenceCosts = friction.RecurrenceCostAnnotations(snap.Signals, open, costs)
	}
	if r.AutoFile {
		rep, err := r.Filer.FileAll(ctx, snap.Signals, r.filingRun(date))
		if err != nil {
			return friction.RenderLinks{}, fmt.Errorf("filing to Kata: %w", err)
		}
		meta.CreatedIssues, meta.TrackerFailures = rep.Issues, rep.Failed
	}
	return r.linksFor(ctx, date, snap.Signals)
}

// linksFor builds the digest issue index from local link rows only; it never
// calls Kata. A read failure is returned so BuildDate cannot publish a digest
// without its issue annotations.
func (r *Runner) linksFor(ctx context.Context, date string, sigs []friction.Signal) (friction.RenderLinks, error) {
	if r.Filer == nil {
		return friction.RenderLinks{}, nil
	}
	links, err := r.Filer.Links(ctx, annotatableFingerprints(sigs))
	if err != nil {
		return friction.RenderLinks{}, fmt.Errorf("loading Kata links: %w", err)
	}
	return friction.RenderLinks{IssueIndex: links}, nil
}

// linksForRerender propagates local link read failures so Rerender preserves
// the existing digest and its issue annotations.
func (r *Runner) linksForRerender(ctx context.Context, date string, sigs []friction.Signal) (friction.RenderLinks, error) {
	return r.linksFor(ctx, date, sigs)
}

// drain is spec §8.2 step 4: failures are logged, never returned.
func (r *Runner) drain(ctx context.Context) {
	if r.Filer == nil {
		return
	}
	if _, err := r.Filer.Drain(ctx); err != nil {
		log.Printf("friction review: draining the Kata outbox: %v", err)
	}
}

// Snapshot returns a stored digest's frozen inputs. The filer uses it to
// rebuild a signal for a drained filing, so the Kata body matches the inline
// filing byte for byte.
func (r *Runner) Snapshot(ctx context.Context, date string) (friction.DigestSnapshot, error) {
	d, err := r.Store.GetFrictionDigest(ctx, date)
	if err != nil {
		return friction.DigestSnapshot{}, err
	}
	if d == nil {
		return friction.DigestSnapshot{}, fmt.Errorf("%w: %s", ErrDigestNotFound, date)
	}
	return DecodeSnapshot(d.SnapshotJSON)
}
