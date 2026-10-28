package review

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidRunDate marks a malformed date or a day not yet complete locally.
var ErrInvalidRunDate = errors.New("invalid friction run date")

// RunRequest is the shared request for the friction run command and API.
type RunRequest struct {
	Date    string
	Rebuild bool
	DryRun  bool
}

// LatestCompleteDate returns yesterday in the runner's time zone. It steps
// back one calendar date rather than 24 hours of wall clock, which a
// skipped midnight would carry two dates back.
func (r *Runner) LatestCompleteDate() string {
	return addDays(localDate(r.now(), r.loc()), -1)
}

// Run catches up when called with no options, or builds one requested date.
// exclusive serializes each day's build with sync, as the scheduled job does;
// nil runs the work directly. onReport receives each written catch-up report,
// or the one requested date's report, so callers keep only what they need.
func (r *Runner) Run(
	ctx context.Context, req RunRequest,
	exclusive func(func() error) error, onReport func(Report),
) error {
	if req.Date == "" && !req.Rebuild && !req.DryRun {
		return r.catchUp(ctx, exclusive, onReport)
	}
	latest := r.LatestCompleteDate()
	date := req.Date
	if date == "" {
		date = latest
	}
	parsed, err := time.Parse(time.DateOnly, date)
	if err != nil || parsed.Format(time.DateOnly) != date {
		return fmt.Errorf("%w: %q is not YYYY-MM-DD", ErrInvalidRunDate, date)
	}
	if date > latest {
		return fmt.Errorf("%w: %s is not a complete local day (latest is %s)", ErrInvalidRunDate, date, latest)
	}
	work := func() error {
		// A preview of a built day keeps that day's sessions, as a rebuild does.
		report, err := r.BuildDate(ctx, date, BuildOptions{Rebuild: req.Rebuild || req.DryRun, DryRun: req.DryRun})
		if err != nil {
			return err
		}
		if onReport != nil {
			onReport(report)
		}
		return nil
	}
	if exclusive == nil {
		return work()
	}
	return exclusive(work)
}
