package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"
	"uuid"

	"github.com/cenkalti/backoff/v7"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/timeutil"
)

// Store is the persistence the review needs (spec §26.5). Both primary
// backends implement it; read-only mirrors do not run reviews.
type Store interface {
	ReadOnly() bool
	FrictionPendingDates(ctx context.Context, first, last string, loc *time.Location) ([]string, error)
	FrictionSubjectsForDate(ctx context.Context, date string, loc *time.Location, includeDigested bool) ([]db.FrictionSubject, error)
	FrictionFindingsForSubjects(ctx context.Context, subjectIDs []string) ([]db.FrictionFinding, error)
	FrictionUsageForSessions(ctx context.Context, sessionIDs []string) (map[string]db.FrictionSessionUsage, error)
	FrictionArchiveSpend(ctx context.Context, from, to string, loc *time.Location) (*db.FrictionArchiveSpendSummary, error)
	SaveFrictionDigest(ctx context.Context, d db.FrictionDigest, subjects []db.FrictionDigestSubject, patterns []db.FrictionPatternUpdate) error
	GetFrictionDigest(ctx context.Context, date string) (*db.FrictionDigest, error)
	LatestFrictionDigestDate(ctx context.Context) (string, error)
	FrictionDigestDates(ctx context.Context, first, last string) ([]string, error)
	EarliestSessionDate(ctx context.Context, loc *time.Location) (string, error)
	UpdateFrictionDigestRender(ctx context.Context, date string, markdown, summaryJSON []byte, revision int) error
}

// DiagnosticSource yields generic diagnostic error signals dated on a local
// date (spec §11.4). Contract: SubjectID is the diagnostic identity, and the
// source never returns an identity already recorded in
// friction_digest_sessions for a different date.
type DiagnosticSource interface {
	DiagnosticSignalsForDate(ctx context.Context, date string, loc *time.Location) ([]friction.Signal, error)
}

// Runner builds digests from stored findings and optional diagnostics.
type Runner struct {
	Store        Store
	Diagnostics  DiagnosticSource
	Loc          *time.Location
	Now          func() time.Time
	BackfillDays int
	SeatPatterns []friction.SeatPattern
}

type BuildOptions struct{ Rebuild, DryRun bool }

type Report struct {
	Date     string
	Snapshot Snapshot
	Written  bool
}

var (
	ErrIncompleteDate = errors.New("friction: date is not complete in the review time zone")
	ErrStaleSubjects  = errors.New("friction: findings not yet computed for some sessions")
)

func (r *Runner) loc() *time.Location {
	if r.Loc == nil {
		return time.UTC
	}
	return r.Loc
}

func (r *Runner) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func sortSubjects(subjects []db.FrictionSubject) {
	sort.SliceStable(subjects, func(i, j int) bool {
		a, b := subjects[i], subjects[j]
		if a.Machine != b.Machine {
			return a.Machine < b.Machine
		}
		if a.FilePath != b.FilePath {
			return a.FilePath < b.FilePath
		}
		return a.SubjectID < b.SubjectID
	})
}

func signalFromFinding(f db.FrictionFinding, s db.FrictionSubject) friction.Signal {
	dims := friction.Dims{Agent: s.Agent, Machine: s.Machine}
	sig := friction.Signal{
		Kind: friction.Kind(f.Kind), SubjectID: f.SessionID, SubjectKind: s.SubjectKind, Dims: dims,
		Detector: f.Detector, Text: f.Text, ToolName: f.ToolName, Label: f.Label,
		Evidence: f.Evidence, Ordinal: f.MessageOrdinal, CallIndex: f.CallIndex, Seq: f.Seq,
	}
	if f.OccurredAt != nil {
		sig.OccurredAt = *f.OccurredAt
	}
	return sig
}

// patternUpdates emits one update per (fingerprint, subject), skipping
// subjects already counted by an earlier build of the same date.
func patternUpdates(date string, sigs []friction.Signal, skip map[string]bool) []db.FrictionPatternUpdate {
	type key struct{ fp, subject string }
	idx := map[key]int{}
	out := []db.FrictionPatternUpdate{}
	for _, s := range sigs {
		if skip[s.SubjectID] {
			continue
		}
		k := key{s.Fingerprint(), s.SubjectID}
		if i, ok := idx[k]; ok {
			out[i].Occurrences++
			if s.Ordinal != nil {
				out[i].Ordinal = s.Ordinal
			}
			continue
		}
		idx[k] = len(out)
		out = append(out, db.FrictionPatternUpdate{
			Fingerprint: k.fp, Kind: string(s.Kind), Title: s.Title(), Date: date,
			SubjectID: s.SubjectID, Ordinal: s.Ordinal, Occurrences: 1,
		})
	}
	return out
}

// diagnosticSignals returns the date's diagnostic signals ordered by
// identity then Seq, forcing SubjectKind. On a rebuild, identities recorded
// by the earlier build are kept even if the source no longer returns them.
// A source error fails the build (the poller retries): skipping would drop
// those diagnostics from a date that is then never rebuilt.
func (r *Runner) diagnosticSignals(
	ctx context.Context, date string, loc *time.Location, prior *Snapshot,
) ([]friction.Signal, error) {
	var out []friction.Signal
	if r.Diagnostics != nil {
		sigs, err := r.Diagnostics.DiagnosticSignalsForDate(ctx, date, loc)
		if err != nil {
			return nil, fmt.Errorf("friction diagnostics for %s: %w", date, err)
		}
		out = append(out, sigs...)
	}
	fresh := map[string]bool{}
	for _, s := range out {
		fresh[s.SubjectID] = true
	}
	if prior != nil {
		for _, s := range prior.Signals {
			if s.SubjectKind == friction.SubjectDiagnostic && !fresh[s.SubjectID] {
				out = append(out, s)
			}
		}
	}
	for i := range out {
		out[i].SubjectKind = friction.SubjectDiagnostic
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SubjectID != out[j].SubjectID {
			return out[i].SubjectID < out[j].SubjectID
		}
		return out[i].Seq < out[j].Seq
	})
	return out, nil
}

// BuildDate ports run_review for one completed local date (spec §8.3).
func (r *Runner) BuildDate(ctx context.Context, date string, opts BuildOptions) (Report, error) {
	// Reload inputs once when a concurrent builder claims membership or a push
	// changes a selected session; leave further contention to the next pass.
	rep, err := backoff.Retry(ctx, func() (Report, error) {
		rep, err := r.buildDate(ctx, date, opts)
		if err != nil && !errors.Is(err, db.ErrFrictionMembershipConflict) && !errors.Is(err, db.ErrFrictionSubjectsChanged) {
			return rep, backoff.Permanent(err)
		}
		return rep, err
	}, backoff.WithBackOff(&backoff.ZeroBackOff{}), backoff.WithMaxTries(2), backoff.WithMaxElapsedTime(0))
	if err != nil {
		return rep, errors.Join(backoff.AsRetryError(err).LastErr, context.Cause(ctx))
	}
	return rep, nil
}

func (r *Runner) buildDate(ctx context.Context, date string, opts BuildOptions) (Report, error) {
	loc := r.loc()
	if !timeutil.IsValidDate(date) {
		return Report{}, fmt.Errorf("friction: invalid date %q (want YYYY-MM-DD)", date)
	}
	if date >= localDate(r.now(), loc) && !opts.DryRun {
		return Report{}, fmt.Errorf("%w: %s", ErrIncompleteDate, date)
	}
	existing, err := r.Store.GetFrictionDigest(ctx, date)
	if err != nil {
		return Report{}, err
	}
	if existing != nil && !opts.Rebuild && !opts.DryRun {
		snap, err := DecodeSnapshot(existing.SnapshotJSON)
		if err != nil {
			return Report{}, err
		}
		return Report{Date: date, Snapshot: snap}, nil
	}

	all, err := r.Store.FrictionSubjectsForDate(ctx, date, loc, opts.Rebuild)
	if err != nil {
		return Report{}, err
	}
	sortSubjects(all)
	subjects := make([]db.FrictionSubject, 0, len(all))
	stale := 0
	for _, s := range all {
		switch {
		case s.RulesVersion == friction.SkippedRulesVersion:
		case r.Store.ReadOnly() && strings.HasSuffix(s.RulesVersion, ":skipped"):
		case r.Store.ReadOnly() && s.RulesVersion == "":
			// A replica cannot review sessions whose source has review disabled.
		case !r.Store.ReadOnly() && s.RulesVersion != friction.RulesVersion:
			stale++
		default:
			subjects = append(subjects, s)
		}
	}
	if stale > 0 && !opts.DryRun {
		return Report{}, fmt.Errorf("%w: %d session(s) on %s", ErrStaleSubjects, stale, date)
	}

	ids := make([]string, len(subjects))
	for i, s := range subjects {
		ids[i] = s.SubjectID
	}
	findings, err := r.Store.FrictionFindingsForSubjects(ctx, ids)
	if err != nil {
		return Report{}, err
	}
	bySubject := map[string][]db.FrictionFinding{}
	for _, f := range findings {
		bySubject[f.SessionID] = append(bySubject[f.SessionID], f)
	}
	usage, err := r.Store.FrictionUsageForSessions(ctx, ids)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Report{}, err
		}
		// load_stats failures only warn (digest.rs:471-491).
		log.Printf("friction review: %s: usage unavailable: %v", date, err)
		usage = nil
	}

	fold := newSpendFold()
	subAgent := map[string]bool{}
	var signals, errorSignals []friction.Signal
	for _, s := range subjects {
		subAgent[s.SubjectID] = s.IsSubAgent
		rows := bySubject[s.SubjectID]
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
		sigs := make([]friction.Signal, 0, len(rows))
		for _, f := range rows {
			sig := signalFromFinding(f, s)
			sig.Dims.Seat = friction.SeatFromPath(s.FilePath, r.SeatPatterns)
			sigs = append(sigs, sig)
			if sig.Kind == friction.KindError {
				errorSignals = append(errorSignals, sig)
			}
		}
		if u, ok := usage[s.SubjectID]; ok {
			u.Role = "(root)"
			if s.IsSubAgent {
				u.Role = "subagent"
			}
			fold.addUsage(u)
		}
		signals = append(signals, sigs...)
	}

	// Diagnostic subjects come last, ordered by identity (spec §8.3 step 1).
	// A rebuild keeps the diagnostic members the first build recorded.
	var prior *Snapshot
	if existing != nil {
		snap, err := DecodeSnapshot(existing.SnapshotJSON)
		if err != nil {
			return Report{}, err
		}
		prior = &snap
	}
	diagnostics, err := r.diagnosticSignals(ctx, date, loc, prior)
	if err != nil {
		return Report{}, err
	}
	diagIDs := []string{}
	for _, sig := range diagnostics {
		if len(diagIDs) == 0 || diagIDs[len(diagIDs)-1] != sig.SubjectID {
			diagIDs = append(diagIDs, sig.SubjectID)
		}
	}
	signals = append(signals, diagnostics...)

	from, to, err := db.FrictionArchiveWindow(date)
	if err != nil {
		return Report{}, err
	}
	archive, err := r.Store.FrictionArchiveSpend(ctx, from, to, loc)
	if err != nil {
		log.Printf("friction review: %s: archive spend hidden: %v", date, err)
		archive = nil
	}

	snap := Snapshot{
		Date: date, Signals: signals,
		P0Alerts:        friction.DetectP0Alerts(errorSignals, func(id string) bool { return subAgent[id] }),
		SessionsScanned: len(subjects) + len(diagIDs),
		Timezone:        loc.String(), RulesVersion: friction.RulesVersion,
		Spend: fold.summary(), ArchiveSpend: archive,
	}
	if opts.DryRun {
		return Report{Date: date, Snapshot: snap}, nil
	}

	md := friction.RenderMarkdown(snap.DigestSnapshot)
	summary := friction.RenderSummaryJSON(snap.DigestSnapshot)
	snapJSON, err := EncodeSnapshot(snap)
	if err != nil {
		return Report{}, err
	}
	runID := uuid.NewV7()
	revision := 1
	counted := map[string]bool{}
	for _, subject := range subjects {
		if subject.AlreadyDigested {
			counted[subject.SubjectID] = true
		}
	}
	if existing != nil {
		revision = existing.Revision + 1
	}
	if prior != nil {
		for _, s := range prior.Signals {
			if s.SubjectKind == friction.SubjectDiagnostic {
				counted[s.SubjectID] = true
			}
		}
	}
	sum := sha256.Sum256(md)
	digest := db.FrictionDigest{
		Date: date, Timezone: loc.String(), RulesVersion: friction.RulesVersion,
		BuiltAt: r.now().UTC(), Revision: revision, SessionsScanned: snap.SessionsScanned,
		SnapshotJSON: snapJSON, SummaryJSON: summary, Markdown: md,
		MarkdownSHA256: hex.EncodeToString(sum[:]), RunID: runID.String(),
	}
	members := make([]db.FrictionDigestSubject, 0, len(subjects)+len(diagIDs))
	for _, s := range subjects {
		members = append(members, db.FrictionDigestSubject{SubjectID: s.SubjectID, Date: date, SubjectKind: s.SubjectKind, SessionVersion: s.SessionVersion})
	}
	for _, id := range diagIDs {
		members = append(members, db.FrictionDigestSubject{SubjectID: id, Date: date, SubjectKind: friction.SubjectDiagnostic})
	}
	err = r.Store.SaveFrictionDigest(ctx, digest, members, patternUpdates(date, signals, counted))
	if errors.Is(err, db.ErrFrictionDigestConflict) {
		log.Printf("friction review: %s: another builder wrote this digest first", date)
		return Report{Date: date, Snapshot: snap}, nil
	}
	if err != nil {
		return Report{}, err
	}
	return Report{Date: date, Snapshot: snap, Written: true}, nil
}

var ErrDigestNotFound = errors.New("friction: no digest for date")

func (r *Runner) backfillDays() int {
	if r.BackfillDays < 1 {
		return 7
	}
	return r.BackfillDays
}

// CatchUp builds missing days and revisits days with unrecorded sessions within
// the backfill window. Pending local reviews defer their date without blocking
// other dates. Each build rechecks subjects under the caller's sync barrier.
func (r *Runner) CatchUp(ctx context.Context) ([]Report, error) {
	var reports []Report
	err := r.catchUp(ctx, nil, func(rep Report) { reports = append(reports, rep) })
	return reports, err
}

// catchUp passes each written report to onBuilt. The scheduled job passes nil
// so each snapshot is dropped once its digest is saved; a long backfill does
// not hold every day's findings in memory.
func (r *Runner) catchUp(
	ctx context.Context, exclusive func(func() error) error, onBuilt func(Report),
) error {
	loc := r.loc()
	today := localDate(r.now(), loc)
	yesterday := addDays(today, -1)
	latest, err := r.Store.LatestFrictionDigestDate(ctx)
	if err != nil {
		return err
	}
	start := addDays(today, -r.backfillDays())
	if latest == "" {
		// Only the initial pass needs the archive's beginning. Once a digest
		// exists, the bounded pending-date query discovers late arrivals.
		earliest, err := r.Store.EarliestSessionDate(ctx, loc)
		if err != nil {
			return err
		}
		if earliest == "" || earliest > yesterday {
			return nil
		}
	}
	if start > yesterday {
		return nil
	}
	pending, err := r.Store.FrictionPendingDates(ctx, start, yesterday, loc)
	if err != nil {
		return err
	}
	// Check every date in the bounded window: later digests do not imply
	// that earlier dates finished review. Even an empty, budget-skipped day
	// must be completed once its pending reviews are resolved.
	dates := map[string]bool{}
	for date := start; date <= yesterday; date = addDays(date, 1) {
		dates[date] = true
	}
	if latest != "" {
		built, err := r.Store.FrictionDigestDates(ctx, start, yesterday)
		if err != nil {
			return err
		}
		for _, date := range built {
			delete(dates, date)
		}
	}
	for _, date := range pending {
		dates[date] = true
	}
	for _, date := range slices.Sorted(maps.Keys(dates)) {
		work := func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			rep, err := r.BuildDate(ctx, date, BuildOptions{Rebuild: true})
			if errors.Is(err, ErrStaleSubjects) {
				log.Printf("friction review: deferring %s: %v", date, err)
				return nil
			}
			if err != nil {
				return fmt.Errorf("building friction digest %s: %w", date, err)
			}
			if rep.Written && onBuilt != nil {
				onBuilt(rep)
			}
			return nil
		}
		if exclusive == nil {
			err = work()
		} else {
			err = exclusive(work)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Rerender rebuilds a digest's Markdown and summary JSON from its frozen
// snapshot and bumps revision when either changed (spec §8.6).
func (r *Runner) Rerender(ctx context.Context, date string) error {
	existing, err := r.Store.GetFrictionDigest(ctx, date)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("%w: %s", ErrDigestNotFound, date)
	}
	snap, err := DecodeSnapshot(existing.SnapshotJSON)
	if err != nil {
		return err
	}
	md := friction.RenderMarkdown(snap.DigestSnapshot)
	summary := friction.RenderSummaryJSON(snap.DigestSnapshot)
	if bytes.Equal(md, existing.Markdown) && bytes.Equal(summary, existing.SummaryJSON) {
		return nil
	}
	return r.Store.UpdateFrictionDigestRender(ctx, date, md, summary, existing.Revision+1)
}
