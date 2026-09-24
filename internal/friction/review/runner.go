package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/timeutil"
)

// Store is the persistence the review needs (spec §26.5). Both primary
// backends implement it; read-only mirrors return db.ErrReadOnly.
type Store interface {
	FrictionSubjectsForDate(ctx context.Context, date string, loc *time.Location, includeDigested bool) ([]db.FrictionSubject, error)
	FrictionFindingsForSubjects(ctx context.Context, subjectIDs []string) ([]db.FrictionFinding, error)
	FrictionUsageForSessions(ctx context.Context, sessionIDs []string) (map[string]friction.SessionUsage, error)
	FrictionArchiveSpend(ctx context.Context, from, to string, loc *time.Location) (*friction.ArchiveSpend, error)
	SaveFrictionDigest(ctx context.Context, d db.FrictionDigest, subjects []db.FrictionDigestSubject, patterns []db.FrictionPatternUpdate) error
	GetFrictionDigest(ctx context.Context, date string) (*db.FrictionDigest, error)
	LatestFrictionDigestDate(ctx context.Context) (string, error)
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

// Runner builds digests. Filer, Ledger and AutoFile from spec §26.5 arrive
// with the Kata and ledger PRs; Diagnostics is nil until PR 17 wires the
// ledger-backed source.
type Runner struct {
	Store        Store
	Diagnostics  DiagnosticSource
	Loc          *time.Location
	Now          func() time.Time
	BackfillDays int
	PublicURL    string
}

type BuildOptions struct{ Rebuild, DryRun bool }

type Report struct {
	Date     string
	Snapshot friction.DigestSnapshot
	Meta     friction.SummaryMeta
	Written  bool
}

var (
	ErrIncompleteDate = errors.New("friction: date is not complete in the review time zone")
	ErrStaleSubjects  = errors.New("friction: findings not yet computed for some sessions")
)

// StaleGrace is how long after a date ends the build waits for sessions
// whose findings are not computed yet before building without them.
const StaleGrace = 48 * time.Hour

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

// DigestURL is summary_json's digest_path for a written digest (spec §9.2).
func DigestURL(publicURL, date string) string {
	if u := strings.TrimRight(strings.TrimSpace(publicURL), "/"); u != "" {
		return u + "/friction/" + date
	}
	return "friction:" + date
}

func (r *Runner) meta(date string, dryRun bool) friction.SummaryMeta {
	if dryRun {
		return friction.SummaryMeta{}
	}
	p := DigestURL(r.PublicURL, date)
	return friction.SummaryMeta{DigestPath: &p}
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
	dims := friction.Dims{Seat: s.Dims.Seat, Agent: s.Agent, Machine: s.Machine}
	if s.Dims.Persona != "" {
		dims.Persona, dims.Channel = s.Dims.Persona, s.Dims.Channel
	}
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
	var out []db.FrictionPatternUpdate
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
	ctx context.Context, date string, loc *time.Location, prior *friction.DigestSnapshot,
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
		return Report{Date: date, Snapshot: snap, Meta: r.meta(date, false)}, nil
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
		case s.Dims.ReviewExcluded:
		case s.RulesVersion != friction.RulesVersion:
			stale++
		default:
			subjects = append(subjects, s)
		}
	}
	if stale > 0 && !opts.DryRun {
		_, dayEnd, err := db.FrictionDayBounds(date, loc)
		if err != nil {
			return Report{}, err
		}
		if r.now().Before(dayEnd.Add(StaleGrace)) {
			return Report{}, fmt.Errorf("%w: %d session(s) on %s", ErrStaleSubjects, stale, date)
		}
		log.Printf("friction review: %s: building without %d session(s) whose findings are not computed", date, stale)
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
			sigs = append(sigs, sig)
			if sig.Kind == friction.KindError {
				errorSignals = append(errorSignals, sig)
			}
		}
		var key *friction.PersonaKey
		if s.Dims.Persona != "" {
			key = &friction.PersonaKey{Persona: s.Dims.Persona, Channel: s.Dims.Channel}
		}
		fold.countSession(key, sigs)
		if u, ok := usage[s.SubjectID]; ok {
			u.Role = "(root)"
			if s.IsSubAgent {
				u.Role = "subagent"
			}
			fold.addUsage(key, u)
		}
		signals = append(signals, sigs...)
	}

	// Diagnostic subjects come last, ordered by identity (spec §8.3 step 1).
	// A rebuild keeps the diagnostic members the first build recorded.
	var prior *friction.DigestSnapshot
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

	snap := friction.DigestSnapshot{
		Date: date, Timezone: loc.String(), RulesVersion: friction.RulesVersion,
		Signals:  signals,
		P0Alerts: friction.DetectP0Alerts(errorSignals, func(id string) bool { return subAgent[id] }),
		Personas: fold.personas, Spend: fold.summary(), ArchiveSpend: archive,
		SessionsScanned: len(subjects) + len(diagIDs),
	}
	meta := r.meta(date, opts.DryRun)
	if opts.DryRun {
		return Report{Date: date, Snapshot: snap, Meta: meta}, nil
	}

	md := friction.RenderMarkdown(snap, friction.RenderLinks{})
	summary := friction.RenderSummaryJSON(snap, meta)
	snapJSON, err := EncodeSnapshot(snap)
	if err != nil {
		return Report{}, err
	}
	runID, err := uuid.NewV7()
	if err != nil {
		return Report{}, fmt.Errorf("friction run id: %w", err)
	}
	revision := 1
	counted := map[string]bool{}
	for _, subject := range subjects {
		if subject.AlreadyDigested {
			counted[subject.SubjectID] = true
		}
	}
	if existing != nil {
		revision = existing.Revision + 1
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
		members = append(members, db.FrictionDigestSubject{SubjectID: s.SubjectID, Date: date, SubjectKind: s.SubjectKind})
	}
	for _, id := range diagIDs {
		members = append(members, db.FrictionDigestSubject{SubjectID: id, Date: date, SubjectKind: friction.SubjectDiagnostic})
	}
	err = r.Store.SaveFrictionDigest(ctx, digest, members, patternUpdates(date, signals, counted))
	if errors.Is(err, db.ErrFrictionDigestConflict) {
		log.Printf("friction review: %s: another builder wrote this digest first", date)
		return Report{Date: date, Snapshot: snap, Meta: meta}, nil
	}
	if err != nil {
		return Report{}, err
	}
	return Report{Date: date, Snapshot: snap, Meta: meta, Written: true}, nil
}

var ErrDigestNotFound = errors.New("friction: no digest for date")

func (r *Runner) backfillDays() int {
	if r.BackfillDays < 1 {
		return 7
	}
	return r.BackfillDays
}

// CatchUp builds every complete local date without a digest, from the
// latest of (last digest + 1, today − backfill_days, earliest session) to
// yesterday. Today is never built. A date still waiting on findings stops
// the pass without error; later dates wait for it so no date is skipped.
func (r *Runner) CatchUp(ctx context.Context) ([]Report, error) {
	loc := r.loc()
	today := localDate(r.now(), loc)
	yesterday := addDays(today, -1)
	earliest, err := r.Store.EarliestSessionDate(ctx, loc)
	if err != nil {
		return nil, err
	}
	if earliest == "" {
		return nil, nil
	}
	start := addDays(today, -r.backfillDays())
	start = max(start, earliest)
	latest, err := r.Store.LatestFrictionDigestDate(ctx)
	if err != nil {
		return nil, err
	}
	if latest != "" {
		start = max(start, addDays(latest, 1))
	}
	var reports []Report
	for d := start; d <= yesterday; d = addDays(d, 1) {
		if err := ctx.Err(); err != nil {
			return reports, err
		}
		rep, err := r.BuildDate(ctx, d, BuildOptions{})
		if errors.Is(err, ErrStaleSubjects) {
			log.Printf("friction review: deferring %s: %v", d, err)
			return reports, nil
		}
		if err != nil {
			return reports, fmt.Errorf("building friction digest %s: %w", d, err)
		}
		if rep.Written {
			reports = append(reports, rep)
		}
	}
	return reports, nil
}

// Rerender rebuilds a digest's Markdown and summary JSON from its frozen
// snapshot and bumps revision when either changed (spec §8.6). Links come
// from the Kata filer once PR 10 adds it; until then none are rendered.
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
	md := friction.RenderMarkdown(snap, friction.RenderLinks{})
	summary := friction.RenderSummaryJSON(snap, r.meta(date, false))
	if bytes.Equal(md, existing.Markdown) && bytes.Equal(summary, existing.SummaryJSON) {
		return nil
	}
	return r.Store.UpdateFrictionDigestRender(ctx, date, md, summary, existing.Revision+1)
}
