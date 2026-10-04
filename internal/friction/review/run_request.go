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

// LatestCompleteDate returns yesterday in the runner's time zone.
func (r *Runner) LatestCompleteDate() string {
	return r.now().In(r.loc()).AddDate(0, 0, -1).Format(time.DateOnly)
}

// Run catches up when called with no options, or builds one requested date.
func (r *Runner) Run(ctx context.Context, req RunRequest) ([]Report, error) {
	if req.Date == "" && !req.Rebuild && !req.DryRun {
		return r.CatchUp(ctx)
	}
	latest := r.LatestCompleteDate()
	date := req.Date
	if date == "" {
		date = latest
	}
	parsed, err := time.Parse(time.DateOnly, date)
	if err != nil || parsed.Format(time.DateOnly) != date {
		return nil, fmt.Errorf("%w: %q is not YYYY-MM-DD", ErrInvalidRunDate, date)
	}
	if date > latest {
		return nil, fmt.Errorf("%w: %s is not a complete local day (latest is %s)", ErrInvalidRunDate, date, latest)
	}
	report, err := r.BuildDate(ctx, date, BuildOptions{Rebuild: req.Rebuild, DryRun: req.DryRun})
	if err != nil {
		return nil, err
	}
	return []Report{report}, nil
}
