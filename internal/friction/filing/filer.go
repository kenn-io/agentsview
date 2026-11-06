package filing

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
)

var (
	// ErrPatternNotFound means no Friction Log pattern has the fingerprint.
	ErrPatternNotFound = errors.New("friction pattern not found")
	// ErrNotFileable means the pattern's kind is never filed to Kata.
	ErrNotFileable = errors.New("friction pattern kind is not filed to Kata")
	// ErrNotLinked means the pattern has no Kata issue link to remove.
	ErrNotLinked = errors.New("friction pattern has no Kata issue link")
	// ErrKataNotReady means the Kata connection is not ready; the error
	// carries its status message.
	ErrKataNotReady = errors.New("kata is not ready")
	// ErrKataDisabled means [kata] is not enabled.
	ErrKataDisabled = errors.New("kata is not configured; set [kata] enabled = true")
	// ErrNotHub means this process pushes its archive to a hub, which files
	// instead, so two machines never file the same pattern.
	ErrNotHub = errors.New("this instance pushes its archive to PostgreSQL; only the AgentsView hub files to Kata")
)

// CheckHub reports whether a Kata connection with cfg may file from here.
func CheckHub(cfg kata.Config) error {
	switch {
	case !cfg.Enabled:
		return ErrKataDisabled
	case !cfg.Hub:
		return ErrNotHub
	}
	return nil
}

// AmbiguousError means Kata already holds more than one issue that could be
// this pattern's. Filing stops so a person can link the right one.
type AmbiguousError struct {
	Candidates []kata.Issue
}

func (e *AmbiguousError) Error() string {
	refs := make([]string, 0, len(e.Candidates))
	for _, is := range e.Candidates {
		refs = append(refs, is.QualifiedID)
	}
	return fmt.Sprintf("more than one Kata issue may track this pattern (%s); link one with `agentsview friction link`",
		strings.Join(refs, ", "))
}

// Kata is the part of the Kata connection filing uses; *kata.Conn
// implements it.
type Kata interface {
	Status(ctx context.Context, fresh bool) kata.Status
	InstanceUID() string
	ProjectUID(ctx context.Context) (string, error)
	FindByMetadata(ctx context.Context, key, value string) ([]kata.Issue, error)
	CreateIssue(ctx context.Context, idempotencyKey string, req kata.CreateIssue) (kata.CreateResult, error)
	GetIssue(ctx context.Context, ref string) (kata.Issue, error)
}

var _ Kata = (*kata.Conn)(nil)

// Store reads patterns and their digests and records issue links.
type Store interface {
	db.FrictionReader
	db.FrictionIssueLinkWriter
}

// Filer files Friction Log patterns to Kata and keeps their links. Callers
// construct it only on the filing hub (EligibleHost).
type Filer struct {
	Kata  Kata
	Store Store
	// PublicURL is AgentsView's public URL including any base path. Issue
	// bodies link back to the session and digest only when it is set.
	PublicURL string
	Now       func() time.Time
	// Exclusive, when set, runs each link and filing-state write. The server passes its
	// archive write barrier, so a write that meets archive maintenance waits
	// for it instead of failing after Kata created the issue.
	Exclusive func(func() error) error
}

// Result is the outcome of filing one pattern.
type Result struct {
	Link db.FrictionIssueLink `json:"link"`
	// Created is true when this call created the Kata issue; false when the
	// pattern was already linked or Kata already held its issue.
	Created bool `json:"created"`
}

func (f *Filer) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Preview returns the create request File would send, without contacting
// Kata. A pattern that is already linked still previews.
func (f *Filer) Preview(ctx context.Context, fingerprint string) (kata.CreateIssue, error) {
	p, err := f.pattern(ctx, fingerprint)
	if err != nil {
		return kata.CreateIssue{}, err
	}
	sig, date, err := f.lastSignal(ctx, p)
	if err != nil {
		return kata.CreateIssue{}, err
	}
	return buildIssue(sig, issueContext{Date: date, PublicURL: f.PublicURL, Pattern: p}), nil
}

// File links the pattern to its Kata issue, creating the issue only when
// Kata holds none for it. A pattern that is already linked returns its
// link, and so does one a person links while File waits on Kata: File only
// adds a link, never replaces one.
func (f *Filer) File(ctx context.Context, fingerprint string) (Result, error) {
	return f.file(ctx, fingerprint, false)
}

// errPaused reports that a person paused the pattern while automatic filing
// worked on it, so the automatic link was not saved.
var errPaused = errors.New("a person paused automatic filing for this pattern")

// file is File; automatic filing sets auto, so its link never undoes a
// pause a person set meanwhile.
func (f *Filer) file(ctx context.Context, fingerprint string, auto bool) (Result, error) {
	p, err := f.pattern(ctx, fingerprint)
	if err != nil {
		return Result{}, err
	}
	if p.Issue != nil {
		return Result{Link: *p.Issue}, nil
	}
	sig, date, err := f.lastSignal(ctx, p)
	if err != nil {
		return Result{}, err
	}
	if err := f.ready(ctx); err != nil {
		return Result{}, err
	}
	matches, err := f.Kata.FindByMetadata(ctx, MetadataFingerprint, fingerprint)
	if err != nil {
		return Result{}, err
	}
	switch len(matches) {
	case 0:
	case 1:
		link, err := f.add(ctx, fingerprint, matches[0], db.FrictionIssueSourceFound, auto)
		return Result{Link: link}, err
	default:
		return Result{}, &AmbiguousError{Candidates: matches}
	}
	req := buildIssue(sig, issueContext{Date: date, PublicURL: f.PublicURL, Pattern: p})
	// The fingerprint names the pattern, so a retried create reuses the
	// issue the first attempt made.
	created, err := f.Kata.CreateIssue(ctx, "friction-"+fingerprint, req)
	if err != nil {
		return f.onCreateError(ctx, fingerprint, err, auto)
	}
	source := db.FrictionIssueSourceCreated
	if created.Reused {
		source = db.FrictionIssueSourceFound
	}
	link, err := f.add(ctx, fingerprint, created.Issue, source, auto)
	return Result{Link: link, Created: !created.Reused && link.IssueUID == created.Issue.UID}, err
}

// onCreateError links the issue Kata names when an earlier create with the
// same key carried different text, and surfaces look-alike candidates.
func (f *Filer) onCreateError(ctx context.Context, fingerprint string, err error, auto bool) (Result, error) {
	apiErr, ok := errors.AsType[*kata.APIError](err)
	if !ok {
		return Result{}, err
	}
	switch apiErr.Code {
	case "idempotency_mismatch":
		var data struct {
			UID string `json:"uid"`
		}
		if json.Unmarshal(apiErr.Data, &data) != nil || data.UID == "" {
			return Result{}, err
		}
		is, gerr := f.Kata.GetIssue(ctx, data.UID)
		if gerr != nil {
			return Result{}, gerr
		}
		link, serr := f.add(ctx, fingerprint, is, db.FrictionIssueSourceFound, auto)
		return Result{Link: link}, serr
	case "duplicate_candidates":
		var data struct {
			Candidates []struct {
				UID         string `json:"uid"`
				QualifiedID string `json:"qualified_id"`
			} `json:"candidates"`
		}
		_ = json.Unmarshal(apiErr.Data, &data)
		amb := &AmbiguousError{}
		for _, c := range data.Candidates {
			amb.Candidates = append(amb.Candidates, kata.Issue{UID: c.UID, QualifiedID: c.QualifiedID})
		}
		return Result{}, amb
	}
	return Result{}, err
}

// Link records a person's choice of Kata issue for the pattern, replacing
// any existing link. ref is a qualified ID such as agentsview#f001, a short
// ID such as f001, or an issue UID.
func (f *Filer) Link(ctx context.Context, fingerprint, ref string) (db.FrictionIssueLink, error) {
	if _, err := f.pattern(ctx, fingerprint); err != nil {
		return db.FrictionIssueLink{}, err
	}
	if err := f.ready(ctx); err != nil {
		return db.FrictionIssueLink{}, err
	}
	is, err := f.Kata.GetIssue(ctx, strings.TrimSpace(ref))
	if err != nil {
		return db.FrictionIssueLink{}, err
	}
	link, err := f.newLink(ctx, fingerprint, is, db.FrictionIssueSourceManual)
	if err != nil {
		return db.FrictionIssueLink{}, err
	}
	return link, f.write(func() error { return f.Store.SaveFrictionIssueLink(ctx, link) })
}

// Unlink removes the local link and pauses automatic filing for the
// pattern, which would otherwise link the same issue again. Filing or
// linking it by hand clears the pause. The Kata issue is left as it is.
func (f *Filer) Unlink(ctx context.Context, fingerprint string) error {
	var removed bool
	err := f.write(func() (err error) {
		removed, err = f.Store.DeleteFrictionIssueLink(ctx, fingerprint)
		return err
	})
	if err != nil {
		return err
	}
	if !removed {
		return ErrNotLinked
	}
	return nil
}

func (f *Filer) ready(ctx context.Context) error {
	if st := f.Kata.Status(ctx, false); st.State != kata.StateReady {
		return fmt.Errorf("%w: %s: %s", ErrKataNotReady, st.State, st.Message)
	}
	return nil
}

func (f *Filer) newLink(ctx context.Context, fingerprint string, is kata.Issue, source string) (db.FrictionIssueLink, error) {
	project, err := f.Kata.ProjectUID(ctx)
	if err != nil {
		return db.FrictionIssueLink{}, err
	}
	return db.FrictionIssueLink{
		Fingerprint: fingerprint, KataInstanceUID: f.Kata.InstanceUID(),
		KataProjectUID: project, IssueUID: is.UID, QualifiedID: is.QualifiedID,
		WebURL: is.WebURL, Source: source, LinkedAt: f.now().UTC(),
	}, nil
}

// add links the pattern to is unless it got a link meanwhile, and returns
// the pattern's link. An automatic add also leaves a paused pattern alone
// and returns errPaused.
func (f *Filer) add(ctx context.Context, fingerprint string, is kata.Issue, source string, auto bool) (db.FrictionIssueLink, error) {
	link, err := f.newLink(ctx, fingerprint, is, source)
	if err != nil {
		return db.FrictionIssueLink{}, err
	}
	var stored db.FrictionIssueLink
	err = f.write(func() (err error) {
		if !auto {
			stored, err = f.Store.AddFrictionIssueLink(ctx, link)
			return err
		}
		var linked bool
		stored, linked, err = f.Store.AddFrictionIssueLinkUnlessPaused(ctx, link)
		if err == nil && !linked {
			err = errPaused
		}
		return err
	})
	return stored, err
}

func (f *Filer) write(work func() error) error {
	if f.Exclusive == nil {
		return work()
	}
	return f.Exclusive(work)
}

func (f *Filer) pattern(ctx context.Context, fingerprint string) (db.FrictionPattern, error) {
	rows, _, err := f.Store.ListFrictionPatterns(ctx, db.FrictionPatternFilter{
		Fingerprints: []string{fingerprint}, Limit: 1,
	})
	if err != nil {
		return db.FrictionPattern{}, err
	}
	if len(rows) == 0 {
		return db.FrictionPattern{}, ErrPatternNotFound
	}
	return rows[0], nil
}

// lastSignal loads the signal a fileable pattern was last seen as, from the
// digest of its last-seen date, and that date.
func (f *Filer) lastSignal(ctx context.Context, p db.FrictionPattern) (friction.Signal, string, error) {
	if !friction.Fileable(friction.Kind(p.Kind)) {
		return friction.Signal{}, "", fmt.Errorf("%w: %s", ErrNotFileable, p.Kind)
	}
	d, err := f.Store.GetFrictionDigest(ctx, p.LastSeenDate)
	if err != nil {
		return friction.Signal{}, "", err
	}
	if d == nil {
		return friction.Signal{}, "", fmt.Errorf("%w: no digest for %s", ErrPatternNotFound, p.LastSeenDate)
	}
	snap, err := review.DecodeSnapshot(d.SnapshotJSON)
	if err != nil {
		return friction.Signal{}, "", err
	}
	sig, ok := lastOccurrence(snap.Signals, p)
	if !ok {
		return friction.Signal{}, "", fmt.Errorf("%w: not in the %s digest", ErrPatternNotFound, d.Date)
	}
	return sig, d.Date, nil
}

// lastOccurrence picks the signal p was last seen as. Error and workaround
// fingerprints span sessions, so a digest can hold several signals for one
// pattern. Recurrence records the subject and ordinal of the last one in
// digest order; when nothing matches both, prefer the subject, then the
// last signal with the fingerprint.
func lastOccurrence(sigs []friction.Signal, p db.FrictionPattern) (friction.Signal, bool) {
	var best friction.Signal
	bestScore := -1
	for _, sig := range sigs {
		if sig.Fingerprint() != p.Fingerprint {
			continue
		}
		score := 0
		if sig.SubjectID == p.LastSubjectID {
			score = 1
			if equalOrdinal(sig.Ordinal, p.LastOrdinal) {
				score = 2
			}
		}
		if score >= bestScore {
			best, bestScore = sig, score
		}
	}
	return best, bestScore >= 0
}

func equalOrdinal(a, b *int) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
