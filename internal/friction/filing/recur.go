package filing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/kata"
)

// Reopen checks recent linked patterns for recurrence after an issue was closed.
func (f *Filer) Reopen(ctx context.Context, since string, loc *time.Location) (int, error) {
	if f.Kata.Status(ctx, false).State != kata.StateReady {
		return 0, nil
	}
	if loc == nil {
		loc = time.UTC
	}
	filter := db.FrictionPatternFilter{Linked: true, Since: since, Limit: db.MaxFrictionListLimit}
	count := 0
	for {
		rows, next, err := f.Store.ListFrictionPatterns(ctx, filter)
		if err != nil {
			return count, err
		}
		for _, p := range rows {
			if err := ctx.Err(); err != nil {
				return count, err
			}
			changed, err := f.reopenIfRecurred(ctx, p, since, loc)
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

func (f *Filer) reopenIfRecurred(ctx context.Context, p db.FrictionPattern, since string, loc *time.Location) (bool, error) {
	is, err := f.Kata.GetIssue(ctx, p.Issue.IssueUID)
	if err != nil {
		return false, err
	}
	if is.Status == "open" || is.ClosedReason != "done" || is.ClosedAt == nil {
		return false, nil
	}
	closed := is.ClosedAt.In(loc).Format(time.DateOnly)
	start := max(closed, since)
	day, err := time.Parse(time.DateOnly, start)
	if err != nil {
		return false, err
	}
	qualified := ""
	for ; day.Format(time.DateOnly) <= p.LastSeenDate; day = day.AddDate(0, 0, 1) {
		date := day.Format(time.DateOnly)
		filter := db.FrictionFindingFilter{Fingerprint: p.Fingerprint, Date: date, Limit: db.MaxFrictionListLimit}
		for {
			findings, next, err := f.Store.ListFrictionFindings(ctx, filter)
			if err != nil {
				return false, err
			}
			for _, finding := range findings {
				if (finding.OccurredAt != nil && finding.OccurredAt.After(*is.ClosedAt)) || (finding.OccurredAt == nil && date > closed) {
					qualified = date
					break
				}
			}
			if qualified != "" || next == "" {
				break
			}
			filter.Cursor = next
		}
		if qualified != "" {
			break
		}
	}
	if qualified == "" {
		return false, nil
	}
	current, err := f.pattern(ctx, p.Fingerprint)
	if err != nil {
		return false, err
	}
	if current.Issue == nil || current.Issue.IssueUID != p.Issue.IssueUID {
		return false, nil
	}
	body := fmt.Sprintf("This pattern recurred on %s after the issue was closed on %s.", qualified, closed)
	if link := friction.DigestURL(f.PublicURL, qualified); link != "" {
		body += "\n\n" + link
	}
	key := "friction-recur-" + p.Issue.IssueUID + "-" + is.ClosedAt.UTC().Format(time.RFC3339)
	if err := f.Kata.Comment(ctx, p.Issue.IssueUID, key, body); err != nil {
		apiErr, ok := errors.AsType[*kata.APIError](err)
		if !ok || apiErr.Code != "idempotency_mismatch" {
			return false, err
		}
	}
	return f.Kata.Reopen(ctx, p.Issue.IssueUID)
}
