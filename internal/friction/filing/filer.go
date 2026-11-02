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
	p, sig, date, err := f.fileablePattern(ctx, fingerprint)
	if err != nil {
		return kata.CreateIssue{}, err
	}
	return buildIssue(sig, issueContext{Date: date, PublicURL: f.PublicURL, Pattern: p}), nil
}

// File links the pattern to its Kata issue, creating the issue only when
// Kata holds none for it. A pattern that is already linked returns its link.
func (f *Filer) File(ctx context.Context, fingerprint string) (Result, error) {
	p, sig, date, err := f.fileablePattern(ctx, fingerprint)
	if err != nil {
		return Result{}, err
	}
	if p.Issue != nil {
		return Result{Link: *p.Issue}, nil
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
		link, err := f.save(ctx, fingerprint, matches[0], db.FrictionIssueSourceFound)
		return Result{Link: link}, err
	default:
		return Result{}, &AmbiguousError{Candidates: matches}
	}
	req := buildIssue(sig, issueContext{Date: date, PublicURL: f.PublicURL, Pattern: p})
	// The fingerprint names the pattern, so a retried create reuses the
	// issue the first attempt made.
	created, err := f.Kata.CreateIssue(ctx, "friction-"+fingerprint, req)
	if err != nil {
		return f.onCreateError(ctx, fingerprint, err)
	}
	source := db.FrictionIssueSourceCreated
	if created.Reused {
		source = db.FrictionIssueSourceFound
	}
	link, err := f.save(ctx, fingerprint, created.Issue, source)
	return Result{Link: link, Created: !created.Reused}, err
}

// onCreateError links the issue Kata names when an earlier create with the
// same key carried different text, and surfaces look-alike candidates.
func (f *Filer) onCreateError(ctx context.Context, fingerprint string, err error) (Result, error) {
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
		link, serr := f.save(ctx, fingerprint, is, db.FrictionIssueSourceFound)
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
	return f.save(ctx, fingerprint, is, db.FrictionIssueSourceManual)
}

// Unlink removes the local link. The Kata issue is left as it is.
func (f *Filer) Unlink(ctx context.Context, fingerprint string) error {
	removed, err := f.Store.DeleteFrictionIssueLink(ctx, fingerprint)
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

func (f *Filer) save(ctx context.Context, fingerprint string, is kata.Issue, source string) (db.FrictionIssueLink, error) {
	project, err := f.Kata.ProjectUID(ctx)
	if err != nil {
		return db.FrictionIssueLink{}, err
	}
	link := db.FrictionIssueLink{
		Fingerprint: fingerprint, KataInstanceUID: f.Kata.InstanceUID(),
		KataProjectUID: project, IssueUID: is.UID, QualifiedID: is.QualifiedID,
		WebURL: is.WebURL, Source: source, LinkedAt: f.now().UTC(),
	}
	return link, f.Store.SaveFrictionIssueLink(ctx, link)
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

// fileablePattern loads the pattern and the signal it was last seen as, from
// the digest of its last-seen date.
func (f *Filer) fileablePattern(ctx context.Context, fingerprint string) (db.FrictionPattern, friction.Signal, string, error) {
	p, err := f.pattern(ctx, fingerprint)
	if err != nil {
		return p, friction.Signal{}, "", err
	}
	if !Fileable(friction.Kind(p.Kind)) {
		return p, friction.Signal{}, "", fmt.Errorf("%w: %s", ErrNotFileable, p.Kind)
	}
	d, err := f.Store.GetFrictionDigest(ctx, p.LastSeenDate)
	if err != nil {
		return p, friction.Signal{}, "", err
	}
	if d == nil {
		return p, friction.Signal{}, "", fmt.Errorf("%w: no digest for %s", ErrPatternNotFound, p.LastSeenDate)
	}
	snap, err := review.DecodeSnapshot(d.SnapshotJSON)
	if err != nil {
		return p, friction.Signal{}, "", err
	}
	for _, sig := range snap.Signals {
		if sig.Fingerprint() == fingerprint {
			return p, sig, d.Date, nil
		}
	}
	return p, friction.Signal{}, "", fmt.Errorf("%w: not in the %s digest", ErrPatternNotFound, d.Date)
}
