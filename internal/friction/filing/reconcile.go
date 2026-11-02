package filing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/stringutil"
)

const (
	retryFirst     = time.Hour
	retryMax       = 24 * time.Hour
	giveUpAfter    = 14 * 24 * time.Hour
	maxStoredError = 500
)

// Policy is what automatic filing considers: patterns of these kinds last
// seen on or after Since.
type Policy struct {
	Kinds []friction.Kind
	Since string
}

// ReconcileReport counts one automatic filing pass.
type ReconcileReport struct {
	Created, Linked, Retry, NeedsPerson int
}

// Reconcile files each recent unlinked pattern the policy covers. It skips
// patterns a person paused or must settle and those waiting to retry, and
// records why a pattern stays unlinked. While Kata is not ready it does
// nothing, so an outage costs no attempts. Because each pass looks at the
// stored patterns afresh, a restart, an outage, or turning automatic filing
// on later needs no catch-up of its own.
func (f *Filer) Reconcile(ctx context.Context, p Policy) (ReconcileReport, error) {
	var rep ReconcileReport
	if f.Kata.Status(ctx, false).State != kata.StateReady {
		return rep, nil
	}
	for _, kind := range p.Kinds {
		cursor := ""
		for {
			rows, next, err := f.Store.ListFrictionPatterns(ctx, db.FrictionPatternFilter{
				Kind: string(kind), Since: p.Since, Limit: db.MaxFrictionListLimit, Cursor: cursor,
			})
			if err != nil {
				return rep, err
			}
			for _, pat := range rows {
				if err := ctx.Err(); err != nil {
					return rep, err
				}
				if !due(pat, f.now()) {
					continue
				}
				res, err := f.File(ctx, pat.Fingerprint)
				switch {
				case err == nil && res.Created:
					rep.Created++
				case err == nil:
					rep.Linked++
				case errors.Is(err, ErrKataNotReady):
					// Kata went away mid-pass; the next pass starts over.
					return rep, nil
				default:
					state, rerr := f.recordFailure(ctx, pat, err)
					if rerr != nil {
						return rep, rerr
					}
					if state == db.FrictionFilingRetry {
						rep.Retry++
					} else {
						rep.NeedsPerson++
					}
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
	return rep, nil
}

func due(p db.FrictionPattern, now time.Time) bool {
	if p.Issue != nil {
		return false
	}
	st := p.Filing
	if st == nil {
		return true
	}
	return st.State == db.FrictionFilingRetry && (st.NextAttemptAt == nil || !now.Before(*st.NextAttemptAt))
}

// recordFailure stores why the pattern stays unlinked and returns the state.
// Failures a retry cannot fix wait for a person; others retry with growing
// delays until they have failed for giveUpAfter.
func (f *Filer) recordFailure(ctx context.Context, p db.FrictionPattern, cause error) (string, error) {
	now := f.now().UTC()
	st := db.FrictionFilingState{
		Fingerprint: p.Fingerprint, State: db.FrictionFilingRetry,
		Attempts: 1, LastError: stringutil.SafeTruncate(cause.Error(), maxStoredError),
		UpdatedAt: now,
	}
	first := now
	if prev := p.Filing; prev != nil && prev.State == db.FrictionFilingRetry {
		st.Attempts = prev.Attempts + 1
		if prev.FirstFailedAt != nil {
			first = *prev.FirstFailedAt
		}
	}
	st.FirstFailedAt = &first
	switch {
	case needsPerson(cause):
		st.State = db.FrictionFilingNeedsPerson
	case now.Sub(first) >= giveUpAfter:
		st.State = db.FrictionFilingNeedsPerson
		st.LastError = stringutil.SafeTruncate(
			fmt.Sprintf("stopped retrying after %d days: %s", int(giveUpAfter.Hours()/24), cause), maxStoredError)
	default:
		next := now.Add(retryDelay(st.Attempts))
		st.NextAttemptAt = &next
	}
	return st.State, f.Store.SaveFrictionFilingState(ctx, st)
}

// retryDelay doubles from retryFirst up to retryMax.
func retryDelay(attempts int) time.Duration {
	d := retryFirst
	for i := 1; i < attempts && d < retryMax; i++ {
		d *= 2
	}
	return min(d, retryMax)
}

// needsPerson reports failures that retrying cannot fix: several candidate
// issues, a missing digest, or a request Kata rejects for its content.
func needsPerson(err error) bool {
	if _, ok := errors.AsType[*AmbiguousError](err); ok {
		return true
	}
	if errors.Is(err, ErrPatternNotFound) || errors.Is(err, ErrNotFileable) ||
		errors.Is(err, kata.ErrIssueInOtherProject) {
		return true
	}
	switch status := kata.StatusOf(err); {
	case status == 0, status == http.StatusUnauthorized, status == http.StatusForbidden,
		status == http.StatusRequestTimeout, status == http.StatusTooManyRequests, status >= 500:
		return false
	default:
		return status >= 400
	}
}
