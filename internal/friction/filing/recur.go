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

// Reopen reopens linked issues closed as done when a digest since `since` holds a finding that occurred after the close.
func (f *Filer) Reopen(ctx context.Context, since string, loc *time.Location) (int, error) {
	if loc == nil {
		loc = time.UTC
	}
	start, err := time.Parse(time.DateOnly, since)
	if err != nil {
		return 0, err
	}
	filter := db.FrictionPatternFilter{Since: since, Limit: db.MaxFrictionListLimit}
	count := 0
	for {
		rows, next, err := f.Store.ListFrictionPatterns(ctx, filter)
		if err != nil {
			return count, err
		}
		for _, p := range rows {
			if p.Issue == nil {
				continue
			}
			if err := ctx.Err(); err != nil {
				return count, err
			}
			changed, err := f.reopenIfRecurred(ctx, p, start, loc)
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
		if next == "" {
			return count, nil
		}
		filter.Cursor = next
	}
}

func (f *Filer) reopenIfRecurred(ctx context.Context, p db.FrictionPattern, start time.Time, loc *time.Location) (bool, error) {
	is, err := f.Kata.GetIssue(ctx, p.Issue.IssueUID)
	if err != nil {
		return false, err
	}
	if is.Status == "open" || is.ClosedReason != "done" || is.ClosedAt == nil {
		return false, nil
	}
	occurrenceDate := ""
	for day := start; day.Format(time.DateOnly) <= p.LastSeenDate; day = day.AddDate(0, 0, 1) {
		date := day.Format(time.DateOnly)
		digest, err := f.Store.GetFrictionDigest(ctx, date)
		if err != nil {
			return false, err
		}
		if digest == nil {
			continue
		}
		snapshot, err := review.DecodeSnapshot(digest.SnapshotJSON)
		if err != nil {
			return false, err
		}
		for _, signal := range snapshot.Signals {
			if signal.Fingerprint() == p.Fingerprint && !signal.OccurredAt.IsZero() && signal.OccurredAt.After(*is.ClosedAt) {
				occurrenceDate = signal.OccurredAt.In(loc).Format(time.DateOnly)
				break
			}
		}
		if occurrenceDate != "" {
			break
		}
	}
	if occurrenceDate == "" {
		return false, nil
	}
	current, err := f.pattern(ctx, p.Fingerprint)
	if err != nil {
		return false, err
	}
	if current.Issue == nil || current.Issue.IssueUID != p.Issue.IssueUID {
		return false, nil
	}
	body := fmt.Sprintf("This pattern recurred on %s.", occurrenceDate)
	key := "friction-recur-" + p.Issue.IssueUID + "-" + is.ClosedAt.UTC().Format(time.RFC3339Nano)
	if err := f.Kata.Comment(ctx, p.Issue.IssueUID, key, body); err != nil {
		apiErr, ok := errors.AsType[*kata.APIError](err)
		if !ok || apiErr.Code != "idempotency_mismatch" {
			return false, err
		}
	}
	return f.Kata.Reopen(ctx, p.Issue.IssueUID)
}
