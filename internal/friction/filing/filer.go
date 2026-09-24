package filing

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/kata"
)

const (
	metaFingerprint = "friction.fingerprint"
	drainBatch      = 50
)

var ErrSignalNotFound = errors.New("friction signal not found in any digest")

type KataAPI interface {
	Ready(ctx context.Context) bool
	InstanceUID() string
	ProjectUID(ctx context.Context) (string, error)
	FindByMetadata(ctx context.Context, key, value string) ([]kata.Issue, error)
	CreateIssue(ctx context.Context, idempotencyKey string, req kata.CreateIssue) (kata.CreateResult, error)
	GetIssue(ctx context.Context, ref string) (kata.Issue, error)
	Reopen(ctx context.Context, ref string) (bool, error)
	Comment(ctx context.Context, ref, idempotencyKey, body string) error
	AddLabel(ctx context.Context, ref, label string) error
}

type LinkStore interface {
	GetFrictionIssueLinks(ctx context.Context, fingerprints []string) (map[string]db.FrictionIssueLink, error)
	UpsertFrictionIssueLink(ctx context.Context, l db.FrictionIssueLink) error
	DeleteFrictionIssueLink(ctx context.Context, fingerprint string) error
	DueFrictionFilings(ctx context.Context, now time.Time, limit int) ([]db.FrictionIssueLink, error)
	DigestDatesForFingerprints(ctx context.Context, fingerprints []string) ([]string, error)
}

type Policy struct {
	AutoFile, ReopenOnRecurrence bool
	Kinds                        []friction.Kind
	Actor                        string
}

type Report struct {
	Created, Linked, Reopened, Failed, NeedsHuman int
	Issues                                        []friction.IssueRef
}

// Filer files Friction Log signals to Kata. It is constructed only on the
// agentsview filing hub (spec §13.0).
type Filer struct {
	Kata      KataAPI
	Store     LinkStore
	Redact    func(string) string
	Now       func() time.Time
	Policy    Policy
	Instance  string
	PublicURL string
	Snapshot  func(ctx context.Context, date string) (friction.DigestSnapshot, error)
	Rerender  func(ctx context.Context, date string) error
}

func (f *Filer) now() time.Time {
	if f.Now != nil {
		return f.Now().UTC()
	}
	return time.Now().UTC()
}

func (f *Filer) redact(s string) string {
	if f.Redact != nil {
		return f.Redact(s)
	}
	return DefaultRedact(s)
}

func (f *Filer) Ready(ctx context.Context) bool { return f.Kata != nil && f.Kata.Ready(ctx) }

// Plan is the exact create request File would send (also the dry-run preview).
func (f *Filer) Plan(sig friction.Signal, run RunContext) kata.CreateIssue {
	return kata.CreateIssue{
		Title:    f.redact(sig.Title()),
		Body:     f.redact(Body(sig, run)),
		Priority: Priority(sig.Kind),
		Labels:   Labels(sig.Kind),
		Metadata: Metadata(sig, run, f.Instance),
		ForceNew: ForceNew(sig) || run.ForceNew,
	}
}

func (f *Filer) existing(ctx context.Context, fp string) (db.FrictionIssueLink, bool, error) {
	links, err := f.Store.GetFrictionIssueLinks(ctx, []string{fp})
	if err != nil {
		return db.FrictionIssueLink{}, false, err
	}
	l, ok := links[fp]
	return l, ok, nil
}

// File ports KataTracker::create (trackers/kata.rs:359-460) onto the Kata API.
// Only a failed attempt returns an error; needs_human is a recorded outcome.
func (f *Filer) File(ctx context.Context, sig friction.Signal, run RunContext) (db.FrictionIssueLink, error) {
	fp := sig.Fingerprint()
	row, has, err := f.existing(ctx, fp)
	if err != nil {
		return db.FrictionIssueLink{}, err
	}
	row.Fingerprint = fp
	if has && !run.ForceNew {
		switch row.State {
		case db.FrictionLinkStateLinked:
			return f.onLinked(ctx, sig, run, row)
		case db.FrictionLinkStateNeedsHuman:
			return row, nil
		}
	}
	if !run.ForceNew {
		matches, err := f.Kata.FindByMetadata(ctx, metaFingerprint, fp)
		if err != nil {
			return f.fail(ctx, row, err)
		}
		switch len(matches) {
		case 0:
		case 1:
			return f.onMatch(ctx, sig, run, row, matches[0])
		default:
			return f.needsHuman(ctx, row, "multiple_matches", fmt.Sprintf("%d Kata issues carry %s=%s", len(matches), metaFingerprint, fp), candidatesJSON(matches, f.redact))
		}
	}
	req := f.Plan(sig, run)
	res, err := f.Kata.CreateIssue(ctx, IdempotencyKey(req.Title), req)
	if err == nil {
		src := db.FrictionLinkSourceCreated
		if res.Reused {
			src = db.FrictionLinkSourceIdempotentReuse
		}
		return f.linked(ctx, row, res.Issue, src, run.Date)
	}
	return f.onCreateError(ctx, row, run, err)
}

// onLinked is the already-linked path. PR 11 adds the recurrence check here.
func (f *Filer) onLinked(_ context.Context, _ friction.Signal, _ RunContext, row db.FrictionIssueLink) (db.FrictionIssueLink, error) {
	return row, nil
}

// onMatch links an existing issue (open or closed). A closed match is linked
// and left untouched; PR 11 adds the done-reopen rule here.
func (f *Filer) onMatch(ctx context.Context, _ friction.Signal, run RunContext, row db.FrictionIssueLink, is kata.Issue) (db.FrictionIssueLink, error) {
	return f.linked(ctx, row, is, db.FrictionLinkSourceFound, run.Date)
}

func (f *Filer) onCreateError(ctx context.Context, row db.FrictionIssueLink, run RunContext, err error) (db.FrictionIssueLink, error) {
	if apiErr, ok := errors.AsType[*kata.APIError](err); ok {
		switch apiErr.Code {
		case "idempotency_mismatch":
			var data struct {
				UID string `json:"uid"`
			}
			if json.Unmarshal(apiErr.Data, &data) == nil && data.UID != "" {
				is, gerr := f.Kata.GetIssue(ctx, data.UID)
				if gerr != nil {
					return f.fail(ctx, row, gerr)
				}
				return f.linked(ctx, row, is, db.FrictionLinkSourceIdempotentReuse, run.Date)
			}
			return f.fail(ctx, row, fmt.Errorf("%w: idempotency_mismatch without data.uid", kata.ErrInvalidResponse))
		case "idempotency_deleted", "federated_read_only", "federation_read_only", "claim_denied", "bootstrap_token_write_forbidden":
			return f.needsHuman(ctx, row, apiErr.Code, apiErr.Error(), "")
		case "duplicate_candidates":
			var data struct {
				Candidates jsontext.Value `json:"candidates"`
			}
			_ = json.Unmarshal(apiErr.Data, &data)
			return f.needsHuman(ctx, row, apiErr.Code, apiErr.Error(), string(data.Candidates))
		}
		switch s := apiErr.Status; {
		case s == http.StatusUnauthorized, s == http.StatusForbidden, s == http.StatusRequestTimeout, s == http.StatusTooManyRequests, s >= 500:
		case s >= 400:
			return f.needsHuman(ctx, row, codeOr(apiErr.Code, fmt.Sprintf("http_%d", s)), apiErr.Error(), "")
		}
	}
	return f.fail(ctx, row, err)
}

func codeOr(code, fallback string) string {
	if code != "" {
		return code
	}
	return fallback
}

func errorCode(err error) string {
	if apiErr, ok := errors.AsType[*kata.APIError](err); ok {
		return codeOr(apiErr.Code, fmt.Sprintf("http_%d", apiErr.Status))
	}
	switch {
	case errors.Is(err, kata.ErrInvalidResponse):
		return "invalid_response"
	case errors.Is(err, kata.ErrNotReady):
		return "not_ready"
	default:
		return "transport"
	}
}

func (f *Filer) linked(ctx context.Context, row db.FrictionIssueLink, is kata.Issue, source, date string) (db.FrictionIssueLink, error) {
	projectUID, _ := f.Kata.ProjectUID(ctx)
	row.State = db.FrictionLinkStateLinked
	row.KataInstanceUID = f.Kata.InstanceUID()
	row.KataProjectUID = projectUID
	row.IssueUID, row.QualifiedID, row.WebURL, row.LinkSource = is.UID, is.QualifiedID, is.WebURL, source
	row.FirstFailedAt, row.NextAttemptAt = nil, nil
	row.LastErrorCode, row.LastError, row.CandidatesJSON = "", "", ""
	if date != "" {
		row.LastRecurrenceDate = date
	}
	row.UpdatedAt = f.now()
	if err := f.Store.UpsertFrictionIssueLink(ctx, row); err != nil {
		return row, err
	}
	return row, nil
}

func (f *Filer) needsHuman(ctx context.Context, row db.FrictionIssueLink, code, msg, candidates string) (db.FrictionIssueLink, error) {
	row.State = db.FrictionLinkStateNeedsHuman
	row.NextAttemptAt = nil
	row.LastErrorCode = code
	row.LastError = boundError(f.redact(msg))
	row.CandidatesJSON = f.redact(candidates)
	row.UpdatedAt = f.now()
	log.Printf("friction: %s needs a human in Kata: %s", row.Fingerprint, row.LastError)
	return row, f.Store.UpsertFrictionIssueLink(ctx, row)
}

// fail records a retryable failure with backoff; past the 14-day cap the row
// is abandoned loudly (reader.rs RETRY_LOOKBACK_CAP_DAYS).
func (f *Filer) fail(ctx context.Context, row db.FrictionIssueLink, cause error) (db.FrictionIssueLink, error) {
	now := f.now()
	row.Attempts++
	if row.FirstFailedAt == nil {
		t := now
		row.FirstFailedAt = &t
	}
	row.LastErrorCode = errorCode(cause)
	row.LastError = boundError(f.redact(cause.Error()))
	row.UpdatedAt = now
	if now.Sub(*row.FirstFailedAt) >= RetryLookbackCapDays*24*time.Hour {
		row.State, row.NextAttemptAt = db.FrictionLinkStateAbandoned, nil
		log.Printf("friction: abandoning Kata filing for %s after %d days of failures; run `agentsview friction file %s` to retry: %s",
			row.Fingerprint, RetryLookbackCapDays, row.Fingerprint, row.LastError)
	} else {
		// Link stores use RFC3339 seconds; return the same retry time they persist.
		next := now.Add(Backoff(row.Attempts, row.Fingerprint)).Truncate(time.Second)
		row.State, row.NextAttemptAt = db.FrictionLinkStateFailed, &next
		log.Printf("friction: Kata filing for %s failed (attempt %d): %s", row.Fingerprint, row.Attempts, row.LastError)
	}
	if err := f.Store.UpsertFrictionIssueLink(ctx, row); err != nil {
		return row, errors.Join(cause, err)
	}
	return row, fmt.Errorf("friction: filing %s: %w", row.Fingerprint, cause)
}

func candidatesJSON(issues []kata.Issue, redact func(string) string) string {
	type cand struct {
		UID         string `json:"uid"`
		QualifiedID string `json:"qualified_id"`
		Title       string `json:"title"`
		Status      string `json:"status"`
	}
	out := make([]cand, 0, len(issues))
	for _, is := range issues {
		out = append(out, cand{is.UID, is.QualifiedID, redact(is.Title), is.Status})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func (f *Filer) kindAllowed(k friction.Kind) bool {
	kinds := f.Policy.Kinds
	if kinds == nil {
		kinds = DefaultKinds()
	}
	return slices.Contains(kinds, k)
}

func shortRef(qualified string) string {
	if _, short, ok := strings.Cut(qualified, "#"); ok {
		return "#" + short
	}
	return "#" + qualified
}

// FileAll files a digest's signals once per fingerprint. With Kata not ready
// it records pending outbox rows and returns an empty report (§13.6).
func (f *Filer) FileAll(ctx context.Context, sigs []friction.Signal, run RunContext) (Report, error) {
	var rep Report
	seen := map[string]bool{}
	var todo []friction.Signal
	for _, s := range sigs {
		fp := s.Fingerprint()
		if seen[fp] || !f.kindAllowed(s.Kind) {
			continue
		}
		seen[fp] = true
		todo = append(todo, s)
	}
	if len(todo) == 0 {
		return rep, nil
	}
	fps := make([]string, 0, len(todo))
	for _, s := range todo {
		fps = append(fps, s.Fingerprint())
	}
	current, err := f.Store.GetFrictionIssueLinks(ctx, fps)
	if err != nil {
		return rep, err
	}
	if !f.Ready(ctx) {
		now := f.now()
		for _, fp := range fps {
			if _, ok := current[fp]; ok {
				continue
			}
			next := now
			if err := f.Store.UpsertFrictionIssueLink(ctx, db.FrictionIssueLink{Fingerprint: fp, State: db.FrictionLinkStatePending, NextAttemptAt: &next, UpdatedAt: now}); err != nil {
				return rep, err
			}
		}
		return rep, nil
	}
	for _, s := range todo {
		if cur, ok := current[s.Fingerprint()]; ok && (cur.State == db.FrictionLinkStateAbandoned || cur.State == db.FrictionLinkStateNeedsHuman) {
			if cur.State == db.FrictionLinkStateNeedsHuman {
				rep.NeedsHuman++
			}
			continue
		}
		link, err := f.File(ctx, s, run)
		f.tally(&rep, s, link, err)
	}
	return rep, nil
}

func (f *Filer) tally(rep *Report, s friction.Signal, link db.FrictionIssueLink, err error) {
	switch link.State {
	case db.FrictionLinkStateLinked:
		if link.LinkSource == db.FrictionLinkSourceCreated {
			rep.Created++
		} else {
			rep.Linked++
		}
		rep.Issues = append(rep.Issues, friction.IssueRef{ID: shortRef(link.QualifiedID), Backend: "kata", URL: link.WebURL, Title: f.redact(s.Title())})
	case db.FrictionLinkStateNeedsHuman:
		rep.NeedsHuman++
	default:
		if err != nil {
			rep.Failed++
		}
	}
}

// SignalForFingerprint finds the signal in the earliest digest containing it,
// so a drained filing sends the same body the inline filing would have.
func (f *Filer) SignalForFingerprint(ctx context.Context, fingerprint string) (friction.Signal, RunContext, error) {
	if f.Snapshot == nil {
		return friction.Signal{}, RunContext{}, ErrSignalNotFound
	}
	dates, err := f.Store.DigestDatesForFingerprints(ctx, []string{fingerprint})
	if err != nil {
		return friction.Signal{}, RunContext{}, err
	}
	for _, date := range dates {
		snap, err := f.Snapshot(ctx, date)
		if err != nil {
			return friction.Signal{}, RunContext{}, err
		}
		for _, s := range snap.Signals {
			if s.Fingerprint() == fingerprint {
				return s, RunContext{Date: date, PublicURL: f.PublicURL, DigestURL: DigestURL(f.PublicURL, date)}, nil
			}
		}
	}
	return friction.Signal{}, RunContext{}, ErrSignalNotFound
}

// FileDate files every signal of an existing digest, then re-renders it.
func (f *Filer) FileDate(ctx context.Context, date string, force bool) (Report, error) {
	if f.Snapshot == nil {
		return Report{}, ErrSignalNotFound
	}
	snap, err := f.Snapshot(ctx, date)
	if err != nil {
		return Report{}, err
	}
	run := RunContext{Date: date, PublicURL: f.PublicURL, DigestURL: DigestURL(f.PublicURL, date), ForceNew: force}
	rep, err := f.FileAll(ctx, snap.Signals, run)
	if err != nil {
		return rep, err
	}
	f.rerender(ctx, []string{date})
	return rep, nil
}

// Link records a manual link to an existing Kata issue.
func (f *Filer) Link(ctx context.Context, fingerprint, issueRef string) (db.FrictionIssueLink, error) {
	if !f.Ready(ctx) {
		return db.FrictionIssueLink{}, kata.ErrNotReady
	}
	is, err := f.Kata.GetIssue(ctx, strings.TrimSpace(issueRef))
	if err != nil {
		return db.FrictionIssueLink{}, err
	}
	row, _, err := f.existing(ctx, fingerprint)
	if err != nil {
		return db.FrictionIssueLink{}, err
	}
	row.Fingerprint = fingerprint
	link, err := f.linked(ctx, row, is, db.FrictionLinkSourceManual, "")
	if err != nil {
		return link, err
	}
	f.rerenderFingerprints(ctx, []string{fingerprint})
	return link, nil
}

// Unlink removes local linkage only and re-renders affected digests.
func (f *Filer) Unlink(ctx context.Context, fingerprint string) error {
	if err := f.Store.DeleteFrictionIssueLink(ctx, fingerprint); err != nil {
		return err
	}
	f.rerenderFingerprints(ctx, []string{fingerprint})
	return nil
}

// Links returns the digest issue index for linked fingerprints (§8.3 step 8).
func (f *Filer) Links(ctx context.Context, fingerprints []string) (map[string]friction.IssueRef, error) {
	rows, err := f.Store.GetFrictionIssueLinks(ctx, fingerprints)
	if err != nil {
		return nil, err
	}
	out := make(map[string]friction.IssueRef, len(rows))
	for fp, l := range rows {
		if l.State == db.FrictionLinkStateLinked {
			out[fp] = friction.IssueRef{ID: shortRef(l.QualifiedID), Backend: "kata", URL: l.WebURL}
		}
	}
	return out, nil
}

// SnapshotOpen reports which linked fingerprints point at an open issue, read
// live before any create (digest.rs:509-541). Used for recurrence annotations.
func (f *Filer) SnapshotOpen(ctx context.Context, fingerprints []string) (map[string]bool, error) {
	rows, err := f.Store.GetFrictionIssueLinks(ctx, fingerprints)
	if err != nil {
		return map[string]bool{}, err
	}
	out := map[string]bool{}
	for fp, l := range rows {
		if l.State != db.FrictionLinkStateLinked || l.IssueUID == "" {
			continue
		}
		is, err := f.Kata.GetIssue(ctx, l.IssueUID)
		if err != nil {
			return map[string]bool{}, err
		}
		if is.Status == "open" {
			out[fp] = true
		}
	}
	return out, nil
}

// Drain retries due pending and failed rows (at most 50), then re-renders the
// digests whose links changed (§13.6, §8.6).
func (f *Filer) Drain(ctx context.Context) (Report, error) {
	var rep Report
	if !f.Ready(ctx) {
		return rep, nil
	}
	now := f.now()
	rows, err := f.Store.DueFrictionFilings(ctx, now, drainBatch)
	if err != nil {
		return rep, err
	}
	var changed []string
	for _, row := range rows {
		if row.State == db.FrictionLinkStateFailed && row.FirstFailedAt != nil && now.Sub(*row.FirstFailedAt) >= RetryLookbackCapDays*24*time.Hour {
			row.State, row.NextAttemptAt, row.UpdatedAt = db.FrictionLinkStateAbandoned, nil, now
			log.Printf("friction: abandoning Kata filing for %s after %d days of failures; run `agentsview friction file %s` to retry",
				row.Fingerprint, RetryLookbackCapDays, row.Fingerprint)
			if err := f.Store.UpsertFrictionIssueLink(ctx, row); err != nil {
				return rep, err
			}
			continue
		}
		sig, run, err := f.SignalForFingerprint(ctx, row.Fingerprint)
		if errors.Is(err, ErrSignalNotFound) {
			if _, herr := f.needsHuman(ctx, row, "signal_not_found", "the signal is no longer in any stored digest", ""); herr != nil {
				return rep, herr
			}
			rep.NeedsHuman++
			continue
		}
		if err != nil {
			log.Printf("friction: drain could not load signal %s: %v", row.Fingerprint, err)
			continue
		}
		link, ferr := f.File(ctx, sig, run)
		f.tally(&rep, sig, link, ferr)
		if link.State == db.FrictionLinkStateLinked {
			changed = append(changed, row.Fingerprint)
		}
	}
	f.rerenderFingerprints(ctx, changed)
	return rep, nil
}

func (f *Filer) rerenderFingerprints(ctx context.Context, fps []string) {
	if len(fps) == 0 {
		return
	}
	dates, err := f.Store.DigestDatesForFingerprints(ctx, fps)
	if err != nil {
		log.Printf("friction: finding digests to re-render: %v", err)
		return
	}
	f.rerender(ctx, dates)
}

func (f *Filer) rerender(ctx context.Context, dates []string) {
	if f.Rerender == nil {
		return
	}
	for _, d := range slices.Compact(slices.Sorted(slices.Values(dates))) {
		if err := f.Rerender(ctx, d); err != nil {
			log.Printf("friction: re-rendering digest %s: %v", d, err)
		}
	}
}
