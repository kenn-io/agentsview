package rawderive

import (
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/rawsync"
)

// RetryPolicy is shared by ordinary worker failures and transaction-owned
// partial projection outcomes. Runtime must supply the same policy to both.
type RetryPolicy struct {
	Base, Maximum time.Duration
	MaxAttempts   int
}
type RetryDecision struct {
	Failed bool
	Delay  time.Duration
}

func (p RetryPolicy) Validate() error {
	if p.Base <= 0 || p.Maximum < p.Base {
		return fmt.Errorf("%w: raw parse worker retry timing is invalid", rawsync.ErrInvalid)
	}
	if p.MaxAttempts <= 0 {
		return fmt.Errorf("%w: raw parse worker max attempts must be positive", rawsync.ErrInvalid)
	}
	return nil
}
func (p RetryPolicy) Decide(attempt int) RetryDecision {
	if attempt >= p.MaxAttempts {
		return RetryDecision{Failed: true}
	}
	return RetryDecision{Delay: retryDelay(attempt, p.Base, p.Maximum)}
}

// CommittedProjectionOutcome reports an incomplete result whose proven content
// and retry/terminal job state have already committed atomically. The worker
// must not perform ordinary failure bookkeeping or let a later heartbeat error
// override this durable outcome.
type CommittedProjectionOutcome uint8

const (
	ProjectionRetrying CommittedProjectionOutcome = iota + 1
	ProjectionFailed
)

func (o CommittedProjectionOutcome) Error() string {
	if o == ProjectionRetrying {
		return "raw projection committed; retry scheduled"
	}
	return "raw projection committed; attempt limit reached"
}
