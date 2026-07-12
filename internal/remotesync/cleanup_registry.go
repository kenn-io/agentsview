package remotesync

import (
	"errors"
	"sync"
)

type cleanupRetrier interface {
	RetryCleanup() error
}

// PendingCleanupError reports that a cleanup retained from an earlier HTTP
// sync still owns resources, so the requested sync callback did not run.
// Err preserves the original operation and cleanup error chain.
type PendingCleanupError struct {
	Err error
}

func (e *PendingCleanupError) Error() string {
	if e == nil || e.Err == nil {
		return "pending HTTP sync cleanup still failed"
	}
	return "pending HTTP sync cleanup still failed: " + e.Err.Error()
}

func (e *PendingCleanupError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// CleanupRegistry serializes HTTP sync work and retains at most one failed
// prepared-source cleanup. A retained cleanup must succeed before later work
// can start, so ownership cannot be overwritten or grow without bound.
type CleanupRegistry struct {
	mu      sync.Mutex
	pending error
}

// Run retries cleanup errors before they can be converted to strings or
// discarded by a caller. The original error is returned unchanged so its
// operation and cleanup causes remain available to errors.Is and errors.As.
func (r *CleanupRegistry) Run(
	run func() (SyncStats, error),
) (SyncStats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.pending != nil {
		if retryCleanup(r.pending) != nil {
			return SyncStats{}, &PendingCleanupError{Err: r.pending}
		}
		r.pending = nil
	}

	stats, err := run()
	if err == nil {
		return stats, nil
	}
	if cleanupErr := retryCleanup(err); cleanupErr != nil {
		r.pending = err
	}
	return stats, err
}

func retryCleanup(err error) error {
	var retrier cleanupRetrier
	if !errors.As(err, &retrier) {
		return nil
	}
	return retrier.RetryCleanup()
}
