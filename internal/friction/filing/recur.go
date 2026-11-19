package filing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
)

// Reopen reopens linked issues closed as done when a saved digest holds a later finding.
func (f *Filer) Reopen(ctx context.Context, since, latest string, loc *time.Location) (int, error) {
	if loc == nil {
		loc = time.UTC
	}
	start, err := time.Parse(time.DateOnly, since)
	if err != nil {
		return 0, err
	}
	end, err := time.Parse(time.DateOnly, latest)
	if err != nil {
		return 0, err
	}
	occurrences := make(map[string]time.Time)
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		digest, err := f.Store.GetFrictionDigest(ctx, day.Format(time.DateOnly))
		if err != nil {
			return 0, err
		}
		if digest == nil {
			continue
		}
		snapshot, err := review.DecodeSnapshot(digest.SnapshotJSON)
		if err != nil {
			return 0, err
		}
		for _, signal := range snapshot.Signals {
			fp := signal.Fingerprint()
			if !signal.OccurredAt.IsZero() && signal.OccurredAt.After(occurrences[fp]) {
				occurrences[fp] = signal.OccurredAt
			}
		}
	}
	fingerprints := make([]string, 0, len(occurrences))
	for fp := range occurrences {
		fingerprints = append(fingerprints, fp)
	}
	count := 0
	for len(fingerprints) > 0 {
		n := min(len(fingerprints), db.MaxFrictionFingerprints)
		filter := db.FrictionPatternFilter{Fingerprints: fingerprints[:n], Limit: db.MaxFrictionFingerprints}
		rows, _, err := f.Store.ListFrictionPatterns(ctx, filter)
		if err != nil {
			return count, err
		}
		for _, p := range rows {
			if p.Issue == nil {
				continue
			}
			occurred := occurrences[p.Fingerprint]
			if !occurred.After(f.EvaluatedOccurrences[p.Issue.IssueUID]) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return count, err
			}
			changed, err := f.reopenIfRecurred(ctx, p, occurred, loc)
			if errors.Is(err, kata.ErrNotReady) || errors.Is(err, kata.ErrUnavailable) || kata.StatusOf(err) >= http.StatusInternalServerError {
				return count, nil
			}
			if err != nil {
				log.Printf("friction: reopen issue %s: %v", p.Issue.IssueUID, err)
				continue
			}
			if changed {
				count++
			}
		}
		fingerprints = fingerprints[n:]
	}
	return count, nil
}

func (f *Filer) reopenIfRecurred(ctx context.Context, p db.FrictionPattern, occurred time.Time, loc *time.Location) (bool, error) {
	is, err := f.Kata.GetIssue(ctx, p.Issue.IssueUID)
	if err != nil {
		return false, err
	}
	if is.Status != "closed" || is.ClosedReason != "done" || is.ClosedAt == nil || !occurred.After(*is.ClosedAt) {
		if f.EvaluatedOccurrences != nil {
			f.EvaluatedOccurrences[p.Issue.IssueUID] = occurred
		}
		return false, nil
	}
	current, err := f.pattern(ctx, p.Fingerprint)
	if err != nil {
		return false, err
	}
	if current.Issue == nil || current.Issue.IssueUID != p.Issue.IssueUID {
		return false, nil
	}
	body := fmt.Sprintf("This pattern recurred on %s.", occurred.In(loc).Format(time.DateOnly))
	key := "friction-recur-" + p.Issue.IssueUID + "-" + is.ClosedAt.UTC().Format(time.RFC3339Nano)
	if err := f.Kata.Comment(ctx, p.Issue.IssueUID, key, body); err != nil {
		apiErr, ok := errors.AsType[*kata.APIError](err)
		if !ok || apiErr.Code != "idempotency_mismatch" {
			return false, err
		}
	}
	changed, err := f.Kata.Reopen(ctx, p.Issue.IssueUID)
	if err == nil && changed && f.EvaluatedOccurrences != nil {
		f.EvaluatedOccurrences[p.Issue.IssueUID] = occurred
	}
	return changed, err
}
