package remotesync

import (
	"errors"
	"reflect"
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

// CleanupRegistry serializes HTTP sync work and retains at most one aggregate
// of failed cleanup owners. Every retained owner must release before later
// work can start, so ownership cannot be overwritten or grow without bound.
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
		if pending := retryCleanup(r.pending); pending != nil {
			r.pending = pending
			return SyncStats{}, &PendingCleanupError{Err: r.pending}
		}
		r.pending = nil
	}

	stats, err := run()
	if err == nil {
		return stats, nil
	}
	if cleanupErr := retryCleanup(err); cleanupErr != nil {
		r.pending = cleanupErr
	}
	return stats, err
}

func retryCleanup(err error) error {
	if retained, ok := err.(*retainedCleanupError); ok {
		if retained.RetryCleanup() != nil {
			return retained
		}
		return nil
	}

	retriers := cleanupRetriers(err)
	if len(retriers) == 0 {
		return nil
	}
	if len(retriers) == 1 {
		if retriers[0].RetryCleanup() != nil {
			return err
		}
		return nil
	}

	retained := &retainedCleanupError{cause: err, retriers: retriers}
	if retained.RetryCleanup() != nil {
		return retained
	}
	return nil
}

type retainedCleanupError struct {
	cause    error
	retriers []cleanupRetrier
}

func (e *retainedCleanupError) Error() string { return e.cause.Error() }
func (e *retainedCleanupError) Unwrap() error { return e.cause }

func (e *retainedCleanupError) RetryCleanup() error {
	remaining := e.retriers[:0]
	var retryErr error
	for _, retrier := range e.retriers {
		if err := retrier.RetryCleanup(); err != nil {
			remaining = append(remaining, retrier)
			retryErr = errors.Join(retryErr, err)
		}
	}
	e.retriers = remaining
	return retryErr
}

func cleanupRetriers(err error) []cleanupRetrier {
	var retriers []cleanupRetrier
	stack := []error{err}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current == nil {
			continue
		}
		if retrier, ok := current.(cleanupRetrier); ok &&
			!containsCleanupRetrier(retriers, retrier) {
			retriers = append(retriers, retrier)
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			stack = append(stack, wrapped.Unwrap()...)
		case interface{ Unwrap() error }:
			stack = append(stack, wrapped.Unwrap())
		}
	}
	return retriers
}

func containsCleanupRetrier(
	retriers []cleanupRetrier, candidate cleanupRetrier,
) bool {
	for _, retrier := range retriers {
		left := reflect.ValueOf(retrier)
		right := reflect.ValueOf(candidate)
		if left.Type() != right.Type() {
			continue
		}
		if left.Type().Comparable() && left.Interface() == right.Interface() {
			return true
		}
		switch left.Kind() {
		case reflect.Chan, reflect.Func, reflect.Pointer,
			reflect.Slice, reflect.UnsafePointer:
			if left.Pointer() == right.Pointer() {
				return true
			}
		}
	}
	return false
}
